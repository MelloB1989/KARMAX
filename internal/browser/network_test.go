package browser

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/MelloB1989/karmax/internal/hostpaths"
)

// testBrowser launches a headless Chromium for a network-capture test, or
// skips when none is on the machine — hermetic and offline either way, but
// this needs a real browser process, not the fake DevTools HTTP servers the
// rest of the package tests against.
func testBrowser(t *testing.T) *Session {
	t.Helper()
	if hostpaths.Browser() == "" {
		t.Skip("no Chrome, Chromium or Edge on this machine")
	}
	s := NewHeadless(t.TempDir())
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := s.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = s.Stop(context.Background()) })
	return s
}

// newCaptureTestServer serves a page that sets a cookie, then — after a
// delay long enough for capture to have attached to the tab — fires a fetch
// to a small JSON endpoint and an XHR to one whose body is over the 1MB cap.
func newCaptureTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		http.SetCookie(w, &http.Cookie{Name: "sid", Value: "test123", Path: "/"})
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, `<!doctype html><script>
setTimeout(function(){
  fetch('/api/data');
  var x = new XMLHttpRequest();
  x.open('GET', '/api/big');
  x.send();
}, 1500);
</script>`)
	})
	mux.HandleFunc("/api/data", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"ok":true,"cookie":%q}`, r.Header.Get("Cookie"))
	})
	mux.HandleFunc("/api/big", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write(bytes.Repeat([]byte("x"), 2<<20)) // 2MB, over the 1MB cap
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// waitFor polls cond until it is true or timeout elapses, failing the test
// otherwise. Capture is asynchronous (a poll loop attaches, a goroutine
// fetches each body), so assertions poll rather than sleep a fixed amount.
func waitFor(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if cond() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("condition never became true")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestNetworkCaptureAndReplay(t *testing.T) {
	s := testBrowser(t)
	srv := newCaptureTestServer(t)
	ctx := context.Background()

	tabs, err := s.Tabs(ctx)
	if err != nil || len(tabs) != 1 {
		t.Fatalf("Tabs: %v, %v (want the one about:blank tab a fresh launch starts with)", tabs, err)
	}
	tabID := tabs[0].ID

	// Wait for capture to actually attach before navigating: this test's
	// whole point is that requests fired after attach are captured, so
	// navigating before Network.enable has landed would test nothing.
	var conn *cdpConn
	waitFor(t, 5*time.Second, func() bool {
		s.netMu.Lock()
		tc, ok := s.attached[tabID]
		s.netMu.Unlock()
		if ok {
			conn = tc.conn
		}
		return ok
	})

	if err := conn.call(ctx, "Page.navigate", map[string]any{"url": srv.URL}, nil); err != nil {
		t.Fatalf("Page.navigate: %v", err)
	}

	var dataReq CapturedRequest
	waitFor(t, 6*time.Second, func() bool {
		for _, r := range s.Requests(RequestFilter{URLContains: "/api/data"}) {
			// Status lands at responseReceived; the body arrives later, off
			// its own goroutine (see fetchResponseBody) — wait for both.
			if r.Status == 200 && (r.ResponseBody != "" || r.BodyOmitted) {
				dataReq = r
				return true
			}
		}
		return false
	})

	t.Run("captures method, type, status and body", func(t *testing.T) {
		if dataReq.Method != "GET" {
			t.Errorf("method = %q, want GET", dataReq.Method)
		}
		if dataReq.ResourceType != "fetch" {
			t.Errorf("resource type = %q, want fetch", dataReq.ResourceType)
		}
		if dataReq.Status != 200 {
			t.Errorf("status = %d, want 200", dataReq.Status)
		}
		if !strings.Contains(dataReq.ResponseBody, `"ok":true`) {
			t.Errorf("response body = %q, want it to contain the JSON payload", dataReq.ResponseBody)
		}
		if dataReq.RequestHeaders["Cookie"] == "" {
			t.Errorf("request headers = %v, want a Cookie header (set by the page's own response)", dataReq.RequestHeaders)
		}
	})

	t.Run("RequestByID returns headers and the JSON body", func(t *testing.T) {
		got, ok := s.RequestByID(dataReq.ID)
		if !ok {
			t.Fatal("RequestByID: not found")
		}
		if got.ResponseBody != dataReq.ResponseBody {
			t.Errorf("body = %q, want %q", got.ResponseBody, dataReq.ResponseBody)
		}
		if got.MimeType != "application/json" {
			t.Errorf("mime type = %q, want application/json", got.MimeType)
		}
		if len(got.ResponseHeaders) == 0 {
			t.Error("expected response headers to be captured")
		}
	})

	t.Run("a body over the 1MB cap is flagged not buffered", func(t *testing.T) {
		var bigReq CapturedRequest
		waitFor(t, 6*time.Second, func() bool {
			for _, r := range s.Requests(RequestFilter{URLContains: "/api/big"}) {
				if r.Status == 200 && r.CompletedAt.Unix() > 0 {
					bigReq = r
					return true
				}
			}
			return false
		})
		if !bigReq.BodyOmitted || bigReq.BodyOmittedReason != "too large" {
			t.Errorf("omitted=%v reason=%q, want omitted with reason \"too large\"", bigReq.BodyOmitted, bigReq.BodyOmittedReason)
		}
		if bigReq.ResponseBody != "" {
			t.Error("a body over the cap must not be buffered")
		}
		if bigReq.BodyBytes <= maxCapturedBodyBytes {
			t.Errorf("body_bytes = %d, want it to reflect the real (over-cap) size", bigReq.BodyBytes)
		}
	})

	t.Run("FetchInTab replay reaches the endpoint with the cookie present", func(t *testing.T) {
		res, err := s.FetchInTab(ctx, tabID, FetchSpec{URL: srv.URL + "/api/data", Method: "GET"})
		if err != nil {
			t.Fatalf("FetchInTab: %v", err)
		}
		if res.Status != 200 {
			t.Fatalf("status = %d, want 200", res.Status)
		}
		var body struct {
			OK     bool   `json:"ok"`
			Cookie string `json:"cookie"`
		}
		if err := json.Unmarshal([]byte(res.Body), &body); err != nil {
			t.Fatalf("decoding replayed body %q: %v", res.Body, err)
		}
		if !strings.Contains(body.Cookie, "test123") {
			t.Errorf("server saw cookie %q, want it to contain the sid the page was given — this process never sent it itself", body.Cookie)
		}
	})
}

// TestNetworkCaptureAttachesToAlreadyRunningBrowser reproduces the live
// topology this package actually ships into: the browser is already running
// before whatever asks for network capture even exists — a daemon restart
// finding the operator's Chrome still up, not a process that just launched
// it. sessionA stands in for the run that launched the browser (a previous
// karmax process, or the daemon before its last restart) and is never asked
// to Start() or Open() again. sessionB is a brand-new Session value — same
// profile dir, same port — that calls exactly what the runtime calls at
// boot (browser.Shared) and nothing else: no Start(), no Open(). A second,
// throwaway CDP client is attached to the same tab first, to mimic a
// DevTools window already being open on it — capture must not assume it is
// the only debugger attached to the target.
func TestNetworkCaptureAttachesToAlreadyRunningBrowser(t *testing.T) {
	if hostpaths.Browser() == "" {
		t.Skip("no Chrome, Chromium or Edge on this machine")
	}
	dir := t.TempDir()
	ctx := context.Background()

	sessionA := NewHeadless(dir)
	startCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := sessionA.Start(startCtx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = sessionA.Stop(context.Background()) })

	tabs, err := sessionA.Tabs(ctx)
	if err != nil || len(tabs) != 1 {
		t.Fatalf("Tabs: %v, %v (want the one about:blank tab a fresh launch starts with)", tabs, err)
	}
	tabID, wsURL := tabs[0].ID, tabs[0].WebSocketDebuggerURL

	devtools, err := dialCDP(ctx, wsURL)
	if err != nil {
		t.Fatalf("dial pretend-devtools client: %v", err)
	}
	defer devtools.Close()

	// The daemon-restart reconnect: a fresh Session value, pointed at the
	// same profile dir, obtained the exact way internal/runtime obtains
	// the one it hands to the "browser" tool.
	sessionB := Shared(dir)
	t.Cleanup(sessionB.stopNetworkCapture)

	srv := newCaptureTestServer(t)

	var conn *cdpConn
	waitFor(t, 5*time.Second, func() bool {
		sessionB.netMu.Lock()
		tc, ok := sessionB.attached[tabID]
		sessionB.netMu.Unlock()
		if ok {
			conn = tc.conn
		}
		return ok
	})

	if err := conn.call(ctx, "Page.navigate", map[string]any{"url": srv.URL}, nil); err != nil {
		t.Fatalf("Page.navigate: %v", err)
	}

	var dataReq CapturedRequest
	waitFor(t, 6*time.Second, func() bool {
		for _, r := range sessionB.Requests(RequestFilter{URLContains: "/api/data"}) {
			if r.Status == 200 && (r.ResponseBody != "" || r.BodyOmitted) {
				dataReq = r
				return true
			}
		}
		return false
	})

	if dataReq.Method != "GET" {
		t.Errorf("method = %q, want GET", dataReq.Method)
	}
	if dataReq.ResourceType != "fetch" {
		t.Errorf("resource type = %q, want fetch", dataReq.ResourceType)
	}
	if dataReq.Status != 200 {
		t.Errorf("status = %d, want 200", dataReq.Status)
	}
	if !strings.Contains(dataReq.ResponseBody, `"ok":true`) {
		t.Errorf("response body = %q, want it to contain the JSON payload", dataReq.ResponseBody)
	}

	got, ok := sessionB.RequestByID(dataReq.ID)
	if !ok {
		t.Fatal("RequestByID: not found")
	}
	if got.ResponseBody != dataReq.ResponseBody {
		t.Errorf("RequestByID body = %q, want %q", got.ResponseBody, dataReq.ResponseBody)
	}
}
