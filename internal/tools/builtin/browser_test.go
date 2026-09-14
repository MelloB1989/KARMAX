package builtin

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/MelloB1989/karmax/internal/browser"
	"github.com/MelloB1989/karmax/internal/hostpaths"
)

// newBrowserToolTestServer serves a page that sets a cookie and, after a
// delay long enough for the tool's browser to have attached its network
// capture, fetches a small JSON endpoint that echoes the cookie back.
func newBrowserToolTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		http.SetCookie(w, &http.Cookie{Name: "sid", Value: "tool-test", Path: "/"})
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, `<!doctype html><script>
setTimeout(function(){ fetch('/api/data'); }, 1500);
</script>`)
	})
	mux.HandleFunc("/api/data", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"ok":true,"cookie":%q}`, r.Header.Get("Cookie"))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// TestBrowserToolRequestsRequestFetchRoundTrip drives the browser tool the
// same way the model's tool loop and POST /api/tools/browser would: through
// Execute, never touching *browser.Session's own methods directly except to
// start/stop it. Launches a real (headless) Chromium — skipped when none is
// on the machine, same guard as internal/browser's own network test.
func TestBrowserToolRequestsRequestFetchRoundTrip(t *testing.T) {
	if hostpaths.Browser() == "" {
		t.Skip("no Chrome, Chromium or Edge on this machine")
	}

	tool := &BrowserTool{Session: browser.NewHeadless(t.TempDir())}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	t.Cleanup(func() { _ = tool.Session.Stop(context.Background()) })

	if res, err := tool.Execute(ctx, map[string]any{"action": "start"}); err != nil || res.IsError {
		t.Fatalf("start: err=%v res=%+v", err, res)
	}

	srv := newBrowserToolTestServer(t)
	if res, err := tool.Execute(ctx, map[string]any{"action": "open", "url": srv.URL}); err != nil || res.IsError {
		t.Fatalf("open: err=%v res=%+v", err, res)
	}

	// The page's setTimeout fires ~1.5s after load; poll for the capture
	// rather than sleeping a fixed amount.
	var reqID string
	deadline := time.Now().Add(6 * time.Second)
	for {
		res, err := tool.Execute(ctx, map[string]any{
			"action": "requests", "url_contains": "/api/data", "method": "GET",
		})
		if err != nil || res.IsError {
			t.Fatalf("requests: err=%v res=%+v", err, res)
		}
		out, _ := res.Output.(map[string]any)
		reqs, _ := out["requests"].([]browser.CapturedRequest)
		for _, r := range reqs {
			if r.Status == 200 && (r.BodyOmitted || r.ResponseBody != "") {
				reqID = r.ID
			}
		}
		if reqID != "" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no /api/data request captured in time")
		}
		time.Sleep(50 * time.Millisecond)
	}

	t.Run("requests filter excludes non-matches", func(t *testing.T) {
		res, err := tool.Execute(ctx, map[string]any{
			"action": "requests", "url_contains": "/api/data", "method": "POST",
		})
		if err != nil || res.IsError {
			t.Fatalf("requests: err=%v res=%+v", err, res)
		}
		out, _ := res.Output.(map[string]any)
		reqs, _ := out["requests"].([]browser.CapturedRequest)
		if len(reqs) != 0 {
			t.Errorf("method=POST filter matched %d requests against a GET-only page, want 0", len(reqs))
		}
	})

	t.Run("request returns the full captured record", func(t *testing.T) {
		res, err := tool.Execute(ctx, map[string]any{"action": "request", "id": reqID})
		if err != nil || res.IsError {
			t.Fatalf("request: err=%v res=%+v", err, res)
		}
		rec, ok := res.Output.(browser.CapturedRequest)
		if !ok {
			t.Fatalf("output type = %T, want browser.CapturedRequest", res.Output)
		}
		if !strings.Contains(rec.ResponseBody, `"ok":true`) {
			t.Errorf("response body = %q, want the JSON payload", rec.ResponseBody)
		}
		if rec.RequestHeaders["Cookie"] == "" {
			t.Error("expected the captured request to carry the Cookie header")
		}
	})

	t.Run("fetch replays inside the tab with the cookie present", func(t *testing.T) {
		res, err := tool.Execute(ctx, map[string]any{
			"action": "fetch", "url": srv.URL + "/api/data", "method": "GET",
		})
		if err != nil || res.IsError {
			t.Fatalf("fetch: err=%v res=%+v", err, res)
		}
		out, _ := res.Output.(map[string]any)
		status, _ := out["status"].(int)
		if status != 200 {
			t.Fatalf("status = %v, want 200", out["status"])
		}
		body, _ := out["body"].(string)
		if !strings.Contains(body, "tool-test") {
			t.Errorf("replayed body = %q, want it to show the server saw the cookie", body)
		}
	})
}
