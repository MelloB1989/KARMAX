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
