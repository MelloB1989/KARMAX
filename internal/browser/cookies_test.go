package browser

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/coder/websocket"
)

func TestCookieForMatchesTheDomainAndItsHosts(t *testing.T) {
	cases := []struct {
		domain, host string
		want         bool
	}{
		{".instagram.com", "www.instagram.com", true},
		{".instagram.com", "instagram.com", true},
		{"www.instagram.com", "www.instagram.com", true},
		{".instagram.com", "notinstagram.com", false},
		{".instagram.com", "instagram.com.evil.test", false},
		{"i.instagram.com", "www.instagram.com", false},
		{"", "www.instagram.com", false},
	}
	for _, c := range cases {
		if got := cookieFor(c.domain, c.host); got != c.want {
			t.Errorf("cookieFor(%q, %q) = %v, want %v", c.domain, c.host, got, c.want)
		}
	}
}

// fakeBrowser answers /json/version and a browser-level DevTools socket that
// knows one command, Storage.getCookies — enough to stand in for Chromium.
func fakeBrowser(t *testing.T, cookies []Cookie) int {
	t.Helper()
	mux := http.NewServeMux()
	var base string
	mux.HandleFunc("/json/version", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{
			"Browser":              "fake/1.0",
			"webSocketDebuggerUrl": "ws" + strings.TrimPrefix(base, "http") + "/devtools/browser/x",
		})
	})
	mux.HandleFunc("/devtools/browser/x", func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		for {
			_, data, err := conn.Read(r.Context())
			if err != nil {
				return
			}
			var req cdpEnvelope
			if json.Unmarshal(data, &req) != nil {
				continue
			}
			reply := map[string]any{"id": req.ID}
			if req.Method == "Storage.getCookies" {
				reply["result"] = map[string]any{"cookies": cookies}
			} else {
				reply["error"] = map[string]any{"code": -32601, "message": "unknown method"}
			}
			b, _ := json.Marshal(reply)
			if conn.Write(r.Context(), websocket.MessageText, b) != nil {
				return
			}
		}
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	base = srv.URL

	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil {
		t.Fatal(err)
	}
	return port
}

func TestCookiesReadsTheJarForOneSite(t *testing.T) {
	port := fakeBrowser(t, []Cookie{
		{Name: "sessionid", Value: "123%3Aabc", Domain: ".instagram.com", HTTPOnly: true},
		{Name: "csrftoken", Value: "tok", Domain: ".instagram.com"},
		{Name: "SID", Value: "google", Domain: ".google.com", HTTPOnly: true},
	})
	s := New(t.TempDir())
	s.mu.Lock()
	s.port = port
	s.mu.Unlock()

	got, err := s.Cookies(context.Background(), "www.instagram.com")
	if err != nil {
		t.Fatalf("Cookies: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d cookies, want the two instagram.com ones: %+v", len(got), got)
	}
	for _, c := range got {
		if c.Domain != ".instagram.com" {
			t.Errorf("a %s cookie leaked into an instagram.com read", c.Domain)
		}
	}
}

func TestCookiesNeedsARunningBrowser(t *testing.T) {
	s := New(t.TempDir())
	if _, err := s.Cookies(context.Background(), "www.instagram.com"); err != ErrNotRunning {
		t.Fatalf("err = %v, want ErrNotRunning", err)
	}
}
