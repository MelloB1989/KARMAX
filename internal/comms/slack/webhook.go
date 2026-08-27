package slack

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"time"

	"github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
	"go.uber.org/zap"
)

// maxHTTPBody bounds what these handlers will read. Slack's payloads are
// small; anything past this is not a request worth parsing.
const maxHTTPBody = 1 << 20 // 1MB

// HandleEvents is the Events API HTTP entry point, for installs that have
// inbound ingress and run without Socket Mode. It shares handleEvent with the
// socket path, so a message routes the same way regardless of transport.
// Unverified requests are rejected outright, never logged-and-processed.
func (c *Channel) HandleEvents(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxHTTPBody))
	if err != nil {
		http.Error(w, "read body", http.StatusBadRequest)
		return
	}
	if err := verifyRequest(r.Header, body, c.secret()); err != nil {
		c.log.Warn("slack events webhook rejected", zap.String("channel", c.id), zap.Error(err))
		http.Error(w, "invalid signature", http.StatusUnauthorized)
		return
	}

	// The one-time handshake Slack requires when a Request URL is first set.
	var probe struct{ Type, Challenge string }
	_ = json.Unmarshal(body, &probe)
	if probe.Type == slackevents.URLVerification {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte(probe.Challenge))
		return
	}

	api, err := slackevents.ParseEvent(json.RawMessage(body), slackevents.OptionNoVerifyToken())
	if err != nil {
		http.Error(w, "bad event", http.StatusBadRequest)
		return
	}

	// No envelope id on this path (that's Socket Mode only); the top-level
	// event_id is the HTTP equivalent for catching a retried delivery.
	dup := false
	if id := callbackEventID(api); id != "" {
		dup = c.dedup.seenBefore(id, time.Now())
	}
	// Acknowledged before any work, same as the socket path — Slack retries
	// an events webhook that doesn't answer within a few seconds.
	w.WriteHeader(http.StatusOK)
	if !dup {
		c.handleEvent(r.Context(), api)
	}
}

// HandleInteraction is the Interactivity HTTP entry point — the fallback for
// Approve/Reject button clicks on an install that isn't on Socket Mode. Same
// verification, same decision path as the socket-delivered version.
func (c *Channel) HandleInteraction(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxHTTPBody))
	if err != nil {
		http.Error(w, "read body", http.StatusBadRequest)
		return
	}
	if err := verifyRequest(r.Header, body, c.secret()); err != nil {
		c.log.Warn("slack interaction webhook rejected", zap.String("channel", c.id), zap.Error(err))
		http.Error(w, "invalid signature", http.StatusUnauthorized)
		return
	}

	// Interactivity payloads arrive form-encoded with the JSON under
	// "payload"; ParseForm needs a re-readable body since we already drained it.
	r.Body = io.NopCloser(bytes.NewReader(body))
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	var cb slack.InteractionCallback
	if err := json.Unmarshal([]byte(r.FormValue("payload")), &cb); err != nil {
		http.Error(w, "bad payload", http.StatusBadRequest)
		return
	}

	w.WriteHeader(http.StatusOK)
	c.handleInteraction(r.Context(), cb)
}

// callbackEventID pulls the top-level event_id Slack assigns an Events API
// delivery, or "" if this isn't a callback event (e.g. a rate-limit notice).
func callbackEventID(api slackevents.EventsAPIEvent) string {
	cb, ok := api.Data.(*slackevents.EventsAPICallbackEvent)
	if !ok {
		return ""
	}
	return cb.EventID
}
