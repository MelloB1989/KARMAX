package slack

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/slack-go/slack"
)

func postSigned(t *testing.T, handler http.HandlerFunc, contentType string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", contentType)
	for k, v := range signedHeader(testSigningSecret, time.Now(), body) {
		req.Header[k] = v
	}
	rec := httptest.NewRecorder()
	handler(rec, req)
	return rec
}

func messageEventBody(eventID string) []byte {
	return []byte(`{
		"token": "verification-token",
		"team_id": "T1",
		"api_app_id": "A1",
		"type": "event_callback",
		"event_id": "` + eventID + `",
		"event_time": 1717171717,
		"event": {
			"type": "message",
			"channel": "C1",
			"user": "U1",
			"text": "hello from a webhook",
			"ts": "1717171717.000100"
		}
	}`)
}

// A bad signature is REJECTED at the HTTP boundary, before anything is parsed
// or handed off — the response is 401, and nothing lands in the inbox.
func TestHandleEventsRejectsBadSignature(t *testing.T) {
	c := newTestChannel(nil)
	c.SetSigningSecret(testSigningSecret)
	body := messageEventBody("Ev1")
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(string(body)))
	for k, v := range signedHeader("wrong-secret", time.Now(), body) {
		req.Header[k] = v
	}
	rec := httptest.NewRecorder()
	c.HandleEvents(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
	select {
	case msg := <-c.IncomingMessages():
		t.Fatalf("an unverified event must not be processed, got %q", msg.Content)
	default:
	}
}

// Slack's URL verification handshake is answered with the challenge, and
// never reaches handleEvent.
func TestHandleEventsAnswersURLVerification(t *testing.T) {
	c := newTestChannel(nil)
	c.SetSigningSecret(testSigningSecret)
	body := []byte(`{"type":"url_verification","challenge":"abc123","token":"x"}`)
	rec := postSigned(t, c.HandleEvents, "application/json", body)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if rec.Body.String() != "abc123" {
		t.Errorf("body = %q, want the echoed challenge", rec.Body.String())
	}
}

// A verified message event is routed to the inbox — the same handleEvent the
// socket path uses.
func TestHandleEventsRoutesAVerifiedMessage(t *testing.T) {
	c := newTestChannel(map[string]string{"U1": "maya"})
	c.SetSigningSecret(testSigningSecret)
	rec := postSigned(t, c.HandleEvents, "application/json", messageEventBody("Ev1"))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	select {
	case msg := <-c.IncomingMessages():
		if msg.Content != "hello from a webhook" {
			t.Errorf("content = %q", msg.Content)
		}
	default:
		t.Fatal("a verified message should have been routed")
	}
}

// The same event_id delivered twice (Slack's own retry behaviour) is only
// processed once.
func TestHandleEventsDedupesByEventID(t *testing.T) {
	c := newTestChannel(map[string]string{"U1": "maya"})
	c.SetSigningSecret(testSigningSecret)
	body := messageEventBody("Ev-dup")

	if rec := postSigned(t, c.HandleEvents, "application/json", body); rec.Code != http.StatusOK {
		t.Fatalf("first delivery: status = %d", rec.Code)
	}
	if rec := postSigned(t, c.HandleEvents, "application/json", body); rec.Code != http.StatusOK {
		t.Fatalf("redelivery: status = %d", rec.Code)
	}

	select {
	case <-c.IncomingMessages():
	default:
		t.Fatal("expected exactly one routed message from the first delivery")
	}
	select {
	case msg := <-c.IncomingMessages():
		t.Fatalf("a redelivered event_id must not be processed twice, got %q", msg.Content)
	default:
	}
}

// A bad signature on the interactivity endpoint is REJECTED the same way.
func TestHandleInteractionRejectsBadSignature(t *testing.T) {
	c := newTestChannel(nil)
	c.SetSigningSecret(testSigningSecret)
	form := url.Values{"payload": {`{"type":"block_actions"}`}}.Encode()
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(form))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	for k, v := range signedHeader("wrong-secret", time.Now(), []byte(form)) {
		req.Header[k] = v
	}
	rec := httptest.NewRecorder()
	c.HandleInteraction(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
	select {
	case d := <-c.Decisions():
		t.Fatalf("an unverified interaction must not produce a decision, got %+v", d)
	default:
	}
}

// A verified Approve click reaches Decisions() with the right proposal id and
// decider — the same mapping decisionFromInteraction is tested against
// directly, exercised here through the actual HTTP path.
func TestHandleInteractionRoutesAVerifiedApproval(t *testing.T) {
	c := newTestChannel(nil)
	c.SetSigningSecret(testSigningSecret)

	cb := slack.InteractionCallback{
		Type: slack.InteractionTypeBlockActions,
		User: slack.User{ID: "U1", Name: "maya"},
	}
	cb.Channel.ID = "C1"
	cb.Message.Timestamp = "1717171700.000100"
	cb.ActionCallback.BlockActions = []*slack.BlockAction{
		{ActionID: approveActionID, Value: "proposal-9"},
	}
	payload, err := cb.MarshalJSON()
	if err != nil {
		t.Fatalf("marshal interaction callback: %v", err)
	}

	form := url.Values{"payload": {string(payload)}}.Encode()
	rec := postSigned(t, c.HandleInteraction, "application/x-www-form-urlencoded", []byte(form))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", rec.Code, rec.Body.String())
	}

	select {
	case d := <-c.Decisions():
		if !d.Approved || d.ProposalID != "proposal-9" || d.DeciderID != "U1" {
			t.Errorf("decision = %+v, want an approval of proposal-9 by U1", d)
		}
	default:
		t.Fatal("expected a decision on the Decisions() channel")
	}
}
