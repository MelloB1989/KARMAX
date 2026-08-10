package tracker

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/MelloB1989/karmax/internal/bus"
	"github.com/MelloB1989/karmax/internal/store"
	"go.uber.org/zap"
)

// testLog gives a durable log backed by a throwaway database, plus a channel
// fed by a subscriber, so a test can assert what actually reached the log.
func testLog(t *testing.T, kinds ...bus.EventKind) (*bus.Log, <-chan bus.Event) {
	t.Helper()
	s, err := store.New(filepath.Join(t.TempDir(), "k.db"), zap.NewNop())
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	t.Cleanup(func() { s.Close() })

	b := bus.NewLog(s, store.DefaultWorkspace, zap.NewNop())
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	got := make(chan bus.Event, 8)
	b.Consume(ctx, "test", kinds, func(_ context.Context, e bus.Event) error {
		got <- e
		return nil
	})
	return b, got
}

func TestParseGitHubIssue(t *testing.T) {
	body := []byte(`{
		"action": "opened",
		"repository": {"full_name": "MelloB1989/karmax"},
		"sender": {"login": "MelloB1989"},
		"issue": {
			"number": 42,
			"title": "wacli drops messages",
			"html_url": "https://github.com/MelloB1989/karmax/issues/42",
			"body": "Sometimes the webhook never fires.",
			"assignee": {"login": "nikhil"}
		}
	}`)
	n := parseGitHub("issues", body)
	if n == nil {
		t.Fatal("expected an event, got nil")
	}
	if n.Ref != "#42" || n.Action != "opened" || n.Assignee != "nikhil" {
		t.Fatalf("bad parse: %+v", n)
	}
	if !strings.Contains(n.summary(), "wacli drops messages") {
		t.Fatalf("summary missing title: %q", n.summary())
	}
}

func TestParseGitHubIgnoresNoise(t *testing.T) {
	// Green builds and unhandled event types must not wake anyone up.
	if n := parseGitHub("workflow_run", []byte(`{"workflow_run":{"name":"ci","conclusion":"success"}}`)); n != nil {
		t.Fatalf("successful build should be ignored, got %+v", n)
	}
	if n := parseGitHub("star", []byte(`{"action":"created"}`)); n != nil {
		t.Fatalf("star should be ignored, got %+v", n)
	}
	// A failure is the case worth surfacing.
	n := parseGitHub("workflow_run", []byte(`{"workflow_run":{"name":"ci","conclusion":"failure"}}`))
	if n == nil || n.Action != "failure" {
		t.Fatalf("failed build should be reported, got %+v", n)
	}
}

func TestParseJiraComment(t *testing.T) {
	body := []byte(`{
		"webhookEvent": "comment_created",
		"user": {"displayName": "Siva"},
		"issue": {
			"key": "CX-17",
			"self": "https://campx.atlassian.net/rest/api/2/issue/10001",
			"fields": {
				"summary": "VAPT report format",
				"project": {"key": "CX"},
				"status": {"name": "In Progress"}
			}
		},
		"comment": {"body": "Can we get this by Friday?", "author": {"displayName": "Srikanth"}}
	}`)
	n := parseJira(body)
	if n == nil {
		t.Fatal("expected an event, got nil")
	}
	if n.Kind != "comment" || n.Actor != "Srikanth" || n.Ref != "CX-17" {
		t.Fatalf("bad parse: %+v", n)
	}
	// The REST self link is useless to a human; it must become a browse link.
	if n.URL != "https://campx.atlassian.net/browse/CX-17" {
		t.Fatalf("bad browse url: %q", n.URL)
	}
}

func TestParseYouTrack(t *testing.T) {
	n := parseYouTrack([]byte(`{
		"issue": {"idReadable":"KAR-3","summary":"Add Slack","url":"https://yt/issue/KAR-3",
		          "project":{"shortName":"KAR"},"reporter":{"fullName":"Nikhil"}},
		"action": "updated"
	}`))
	if n == nil || n.Ref != "KAR-3" || n.Project != "KAR" {
		t.Fatalf("bad parse: %+v", n)
	}
	if parseYouTrack([]byte(`{"foo":1}`)) != nil {
		t.Fatal("payload with no issue should be dropped")
	}
}

func TestGitHubSignatureRequired(t *testing.T) {
	h := &Handler{cfg: Config{Source: GitHub, Secret: "s3cret"}}
	body := []byte(`{"action":"opened"}`)

	mac := hmac.New(sha256.New, []byte("s3cret"))
	mac.Write(body)
	good := "sha256=" + hex.EncodeToString(mac.Sum(nil))

	r := httptest.NewRequest("POST", "/hooks/github", nil)
	r.Header.Set("X-Hub-Signature-256", good)
	if !h.authorized(r, body) {
		t.Fatal("valid signature was rejected")
	}

	r.Header.Set("X-Hub-Signature-256", "sha256=deadbeef")
	if h.authorized(r, body) {
		t.Fatal("forged signature was accepted")
	}

	r.Header.Del("X-Hub-Signature-256")
	if h.authorized(r, body) {
		t.Fatal("unsigned delivery was accepted")
	}
}

// End-to-end: a signed delivery must reach the bus as one usable event, and an
// unsigned one must not.
func TestServeHTTPPublishesToBus(t *testing.T) {
	b, got := testLog(t, EventKind)

	h := New(Config{Source: GitHub, Secret: "s3cret", AgentID: "nexus"}, b, zap.NewNop())

	body := []byte(`{"action":"opened","repository":{"full_name":"MelloB1989/karmax"},
	                 "sender":{"login":"MelloB1989"},
	                 "issue":{"number":7,"title":"tracker events","html_url":"https://x/7"}}`)
	mac := hmac.New(sha256.New, []byte("s3cret"))
	mac.Write(body)

	req := httptest.NewRequest("POST", "/hooks/github", bytes.NewReader(body))
	req.Header.Set("X-GitHub-Event", "issues")
	req.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	select {
	case evt := <-got:
		if evt.AgentID != "nexus" {
			t.Errorf("event addressed to %q, want nexus", evt.AgentID)
		}
		summary, _ := evt.Payload["summary"].(string)
		if !strings.Contains(summary, "#7") {
			t.Errorf("summary missing the issue ref: %q", summary)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no event published")
	}

	// Now an unsigned delivery: rejected, and nothing on the bus.
	bad := httptest.NewRequest("POST", "/hooks/github", bytes.NewReader(body))
	bad.Header.Set("X-GitHub-Event", "issues")
	w2 := httptest.NewRecorder()
	h.ServeHTTP(w2, bad)

	if w2.Code != 401 {
		t.Fatalf("unsigned delivery should be 401, got %d", w2.Code)
	}
	select {
	case evt := <-got:
		t.Fatalf("unsigned delivery reached the bus: %+v", evt.Payload)
	case <-time.After(200 * time.Millisecond):
	}
}

func TestSharedTokenAuth(t *testing.T) {
	h := &Handler{cfg: Config{Source: Jira, Secret: "tok"}}
	if !h.authorized(httptest.NewRequest("POST", "/hooks/jira?token=tok", nil), nil) {
		t.Fatal("correct token rejected")
	}
	if h.authorized(httptest.NewRequest("POST", "/hooks/jira?token=nope", nil), nil) {
		t.Fatal("wrong token accepted")
	}
}
