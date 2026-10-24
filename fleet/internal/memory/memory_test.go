package memory

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

const key = "gl_test_key_0123456789"

// gitloom is a fake gitloom that records what it is asked, and answers every
// request with the Authorization header it got in its body: whatever reaches
// a caller with that header in it leaks the key.
type gitloom struct {
	mu     sync.Mutex
	reqs   []string
	status int
	body   string
}

func (g *gitloom) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	g.mu.Lock()
	g.reqs = append(g.reqs, r.Method+" "+r.URL.Path+"?"+r.URL.RawQuery+" auth="+r.Header.Get("Authorization")+" "+string(b))
	status, body := g.status, g.body
	g.mu.Unlock()
	if status == 0 {
		status = http.StatusOK
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, strings.ReplaceAll(body, "$AUTH", r.Header.Get("Authorization")))
}

func newClient(t *testing.T, g *gitloom) *Client {
	t.Helper()
	srv := httptest.NewServer(g)
	t.Cleanup(srv.Close)
	return &Client{URL: srv.URL, Key: key, Namespace: "mira"}
}

func TestRecallSearchesTheNamespaceWithTheKeyAndReturnsTheMemories(t *testing.T) {
	g := &gitloom{body: `{"namespace":"mira","query":"ram","memories":[` +
		`{"path":"people/ram.md","title":"Ram","topic":"people","tier":"long","content":"Ram runs the build host.","score":0.91,"updated_at":"2026-10-01T10:00:00Z"}]}`}
	c := newClient(t, g)
	res, err := c.Recall(context.Background(), "who is ram", 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Memories) != 1 || res.Memories[0].Path != "people/ram.md" || res.Memories[0].Content != "Ram runs the build host." ||
		res.Memories[0].Score != 0.91 || res.Namespace != "mira" || res.Query != "who is ram" {
		t.Fatalf("result = %+v", res)
	}
	if len(g.reqs) != 1 {
		t.Fatalf("requests = %q", g.reqs)
	}
	got := g.reqs[0]
	for _, want := range []string{"GET /v1/retrieve?", "namespace=mira", "q=who+is+ram", "limit=3", "auth=Bearer " + key} {
		if !strings.Contains(got, want) {
			t.Errorf("request %q lacks %q", got, want)
		}
	}
}

func TestRecallNeedsAQueryAndKeepsTheLimitSane(t *testing.T) {
	g := &gitloom{body: `{"memories":[]}`}
	c := newClient(t, g)
	if _, err := c.Recall(context.Background(), "  ", 5); err == nil {
		t.Fatal("an empty query was sent")
	}
	if _, err := c.Recall(context.Background(), "x", 0); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Recall(context.Background(), "x", 500); err != nil {
		t.Fatal(err)
	}
	if len(g.reqs) != 2 || !strings.Contains(g.reqs[0], "limit=5") || !strings.Contains(g.reqs[1], "limit=20") {
		t.Fatalf("requests = %q", g.reqs)
	}
}

func TestTheKeyNeverReachesTheCaller(t *testing.T) {
	for name, g := range map[string]*gitloom{
		"in a result":   {body: `{"query":"q","memories":[{"path":"a.md","content":"seen: $AUTH"}]}`},
		"in an error":   {status: http.StatusUnauthorized, body: `{"error":{"code":"bad_key","message":"key $AUTH is not valid"}}`},
		"in a bad body": {status: http.StatusBadGateway, body: `upstream said $AUTH`},
	} {
		t.Run(name, func(t *testing.T) {
			c := newClient(t, g)
			res, err := c.Recall(context.Background(), "q", 5)
			var all string
			if err != nil {
				all += err.Error()
			}
			if res != nil {
				for _, m := range res.Memories {
					all += m.Content + m.Path + m.Title
				}
			}
			if all == "" {
				t.Fatal("nothing came back to check")
			}
			if strings.Contains(all, key) {
				t.Fatalf("the key reached the caller: %q", all)
			}
		})
	}
}

func TestAnErrorFromGitloomSaysWhatWentWrong(t *testing.T) {
	g := &gitloom{status: http.StatusNotFound, body: `{"error":{"code":"namespace_not_found","message":"no namespace mira"}}`}
	_, err := newClient(t, g).Recall(context.Background(), "q", 5)
	if err == nil || !strings.Contains(err.Error(), "404") || !strings.Contains(err.Error(), "no namespace mira") {
		t.Fatalf("err = %v", err)
	}
}

func TestOnlyTheReadRoutesGoOutWithTheKey(t *testing.T) {
	g := &gitloom{body: `{}`}
	srv := httptest.NewServer(g)
	defer srv.Close()
	hc := &http.Client{Transport: readOnly{key: key, next: http.DefaultTransport}}
	for _, r := range []struct{ method, path string }{
		{"POST", "/v1/memories"}, {"DELETE", "/v1/memories?path=a.md"}, {"PUT", "/v1/retrieve"},
		{"GET", "/v1/namespaces"}, {"GET", "/v1/retrieve/../memories"},
	} {
		req, _ := http.NewRequest(r.method, srv.URL+r.path, strings.NewReader(`{}`))
		if resp, err := hc.Do(req); err == nil {
			resp.Body.Close()
			t.Errorf("%s %s went out", r.method, r.path)
		}
	}
	for _, path := range []string{"/v1/retrieve?q=x", "/v1/memories?path=a.md"} {
		req, _ := http.NewRequest("GET", srv.URL+path, nil)
		resp, err := hc.Do(req)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		resp.Body.Close()
	}
	if len(g.reqs) != 2 {
		t.Fatalf("gitloom saw %q", g.reqs)
	}
	for _, r := range g.reqs {
		if !strings.HasPrefix(r, "GET ") {
			t.Errorf("gitloom saw %q", r)
		}
	}
}
