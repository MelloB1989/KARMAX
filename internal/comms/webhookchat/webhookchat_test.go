package webhookchat

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.uber.org/zap"
)

type capture struct {
	urls   []string
	bodies []map[string]any
}

func (c *capture) server(t *testing.T) *httptest.Server {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var m map[string]any
		_ = json.Unmarshal(raw, &m)
		c.urls = append(c.urls, r.URL.String())
		c.bodies = append(c.bodies, m)
	}))
	t.Cleanup(s.Close)
	return s
}

func TestGoogleChatThreadsByKey(t *testing.T) {
	var c capture
	srv := c.server(t)
	ch := New("gchat", map[string]string{"preset": "googlechat", "url": srv.URL + "/v1/spaces/AAA/messages?key=k"}, zap.NewNop())
	thread, err := ch.PostThread(context.Background(), "", "", "🚨 incident")
	if err != nil || thread == "" {
		t.Fatalf("thread %q err %v", thread, err)
	}
	if _, err := ch.PostThread(context.Background(), "", thread, "update"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(c.urls[1], "threadKey="+thread) || !strings.Contains(c.urls[1], "key=k") {
		t.Fatalf("second post not in the thread: %s", c.urls[1])
	}
	if c.bodies[0]["text"] != "🚨 incident" {
		t.Fatalf("body %v", c.bodies[0])
	}
}

func TestNamedTargetsAndDiscordSplit(t *testing.T) {
	var c capture
	srv := c.server(t)
	ch := New("dc", map[string]string{"preset": "discord", "url.alerts": srv.URL + "/alerts"}, zap.NewNop())
	if err := ch.Send(context.Background(), "#alerts", strings.Repeat("x", 4000)); err != nil {
		t.Fatal(err)
	}
	if len(c.bodies) != 3 || !strings.HasSuffix(c.urls[0], "/alerts") || c.bodies[0]["content"] == nil {
		t.Fatalf("got %d posts to %v", len(c.bodies), c.urls)
	}
	if err := ch.Send(context.Background(), "nowhere", "hi"); err == nil {
		t.Fatal("an unknown target with no default url must fail")
	}
}

func TestInboundUnderstandsGoogleChatAndGenericJSON(t *testing.T) {
	ch := New("gchat", map[string]string{"preset": "googlechat", "inbound_token": "tok"}, zap.NewNop())
	post := func(body, token string) int {
		req := httptest.NewRequest("POST", "/comms/gchat?token="+token, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		ch.HandleInbound(w, req)
		return w.Code
	}
	if post(`{"text":"x"}`, "bad") != 401 {
		t.Fatal("token not enforced")
	}
	post(`{"type":"MESSAGE","message":{"name":"m1","text":"@KARMAX approve K7F2Q","argumentText":" approve K7F2Q",
	   "sender":{"displayName":"Maya","name":"users/1","type":"HUMAN"},"space":{"name":"spaces/AAA"}}}`, "tok")
	post(`{"text":"status","user":"raj","channel":"ops"}`, "tok")
	post(`{"type":"ADDED_TO_SPACE","space":{"name":"spaces/AAA"}}`, "tok")
	m1, m2 := <-ch.IncomingMessages(), <-ch.IncomingMessages()
	if m1.Content != "approve K7F2Q" || m1.SenderName != "Maya" || m1.ChannelID != "spaces/AAA" {
		t.Fatalf("google chat: %+v", m1)
	}
	if m2.Content != "status" || m2.ChannelID != "ops" || m2.SenderName != "raj" {
		t.Fatalf("generic: %+v", m2)
	}
	select {
	case m := <-ch.IncomingMessages():
		t.Fatalf("a space event is not a message: %+v", m)
	default:
	}
}

func TestGenericPostThreadSendsAndReturnsThreadKey(t *testing.T) {
	var c capture
	srv := c.server(t)
	ch := New("bridge", map[string]string{"url": srv.URL}, zap.NewNop())
	thread, err := ch.PostThread(context.Background(), "", "", "🚨 incident")
	if err != nil || thread == "" {
		t.Fatalf("thread %q err %v", thread, err)
	}
	if c.bodies[0]["thread"] != thread {
		t.Fatalf("first post must carry the key it returns: %v", c.bodies[0])
	}
	if got, err := ch.PostThread(context.Background(), "", thread, "update"); err != nil || got != thread {
		t.Fatalf("rejoin returned %q err %v", got, err)
	}
	if c.bodies[1]["thread"] != thread {
		t.Fatalf("second post not in the thread: %v", c.bodies[1])
	}
	if err := ch.Send(context.Background(), "", "plain"); err != nil {
		t.Fatal(err)
	}
	if _, has := c.bodies[2]["thread"]; has {
		t.Fatalf("a plain Send is not threaded: %v", c.bodies[2])
	}
}

func TestOtherPresetsPayloadsUnchanged(t *testing.T) {
	var c capture
	srv := c.server(t)
	ch := New("sl", map[string]string{"preset": "slack", "url": srv.URL}, zap.NewNop())
	if got, _ := ch.PostThread(context.Background(), "", "", "hi"); got != "" {
		t.Fatalf("slack preset returns no thread, got %q", got)
	}
	if len(c.bodies[0]) != 1 {
		t.Fatalf("slack body %v", c.bodies[0])
	}
}

func TestInboundFormContentTypeWithJSONBody(t *testing.T) {
	ch := New("bridge", map[string]string{"inbound_token": "tok"}, zap.NewNop())
	req := httptest.NewRequest("POST", "/comms/bridge?token=tok", strings.NewReader(`{"text":"hello","user":"raj","thread":"T1"}`))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded") // what curl -d sends
	w := httptest.NewRecorder()
	ch.HandleInbound(w, req)
	if w.Code != 200 {
		t.Fatalf("code %d: %s", w.Code, w.Body)
	}
	select {
	case m := <-ch.IncomingMessages():
		if m.Content != "hello" || m.SenderName != "raj" || m.ReplyToID != "T1" {
			t.Fatalf("%+v", m)
		}
	default:
		t.Fatal("message silently dropped")
	}
}

func TestInboundFormEncodedStillWorks(t *testing.T) {
	ch := New("mm", map[string]string{"preset": "mattermost"}, zap.NewNop())
	req := httptest.NewRequest("POST", "/comms/mm", strings.NewReader("text=hi+there&user_name=raj&channel_name=ops"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	ch.HandleInbound(w, req)
	m := <-ch.IncomingMessages()
	if w.Code != 200 || m.Content != "hi there" || m.ChannelID != "ops" {
		t.Fatalf("%d %+v", w.Code, m)
	}
}

func TestInboundWithoutMessageIsRejected(t *testing.T) {
	ch := New("bridge", nil, zap.NewNop())
	for name, tc := range map[string][2]string{
		"empty json":   {"application/json", `{}`},
		"no text":      {"application/json", `{"user":"raj"}`},
		"not json":     {"application/json", `hello`},
		"form no text": {"application/x-www-form-urlencoded", `user_name=raj`},
		"bare text":    {"application/x-www-form-urlencoded", `just words`},
	} {
		req := httptest.NewRequest("POST", "/comms/bridge", strings.NewReader(tc[1]))
		req.Header.Set("Content-Type", tc[0])
		w := httptest.NewRecorder()
		ch.HandleInbound(w, req)
		if w.Code/100 != 4 || !strings.Contains(w.Body.String(), "error") {
			t.Errorf("%s: want 4xx with a reason, got %d %q", name, w.Code, w.Body)
		}
	}
	// A platform handshake and a non-message event are still acknowledged.
	req := httptest.NewRequest("POST", "/comms/bridge", strings.NewReader(`{"challenge":"abc"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	ch.HandleInbound(w, req)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "abc") {
		t.Fatalf("challenge: %d %s", w.Code, w.Body)
	}
}
