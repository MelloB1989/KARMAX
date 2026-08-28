package webhook

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/MelloB1989/karmax/internal/bus"
	"github.com/MelloB1989/karmax/internal/store"
	"github.com/MelloB1989/karmax/internal/tracker"
	"go.uber.org/zap"
)

// Operator-defined webhooks, dispatched from a lookup rather than the mux.
//
// AddRoute registers a pattern directly, and Go's ServeMux panics if the same
// pattern is registered twice — so a config-file route can never be edited
// while the daemon runs. One handler mounted at a prefix, resolving the
// endpoint per delivery, is what lets an operator create, re-secret or delete a
// webhook from the console and have it take effect on the very next request.

// CustomPrefix is where operator-defined webhooks live.
//
// Under /hooks like everything else a third party posts to, so the same CDN
// rule carries it and nobody has to remember a second one. "in" rather than
// "custom" because these are not all custom: a GitHub endpoint created here is
// decoded by KARMAX, and /hooks/custom/github-main would say the opposite.
const CustomPrefix = "/hooks/in/"

// maxBody is what will be read from a delivery. Generous for a webhook and
// bounded, so a sender that streams forever cannot exhaust memory.
const maxBody = 2 << 20

// CustomDispatcher resolves and runs operator-defined webhooks.
type CustomDispatcher struct {
	store *store.Store
	bus   *bus.Log
	log   *zap.Logger
}

// NewCustomDispatcher builds the handler for CustomPrefix.
func NewCustomDispatcher(s *store.Store, b *bus.Log, log *zap.Logger) *CustomDispatcher {
	return &CustomDispatcher{store: s, bus: b, log: log}
}

// Mount registers the dispatcher.
func (d *CustomDispatcher) Mount(add func(pattern string, h http.HandlerFunc)) {
	add(CustomPrefix+"{slug}", d.serve)
	d.log.Info("custom webhook dispatcher mounted", zap.String("prefix", CustomPrefix))
}

func (d *CustomDispatcher) serve(w http.ResponseWriter, r *http.Request) {
	slug := strings.Trim(r.PathValue("slug"), "/")

	ep, err := d.store.WebhookEndpointBySlug(slug)
	if err != nil {
		d.log.Error("could not look up a webhook endpoint", zap.String("slug", slug), zap.Error(err))
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if ep == nil {
		// Deliberately not recorded: an unknown slug has no endpoint to record
		// it against, and writing a row per probe would let anyone fill the
		// table by guessing paths.
		http.Error(w, "no such webhook", http.StatusNotFound)
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, maxBody))
	if err != nil {
		d.record(ep, r, "error", "could not read the request body", nil)
		http.Error(w, "could not read body", http.StatusBadRequest)
		return
	}

	if !ep.Enabled {
		// Recorded, because "I disabled it and deliveries stopped" and "it
		// broke" look the same to whoever is debugging six weeks later.
		d.record(ep, r, "disabled", "the endpoint is turned off", body)
		http.Error(w, "this webhook is disabled", http.StatusServiceUnavailable)
		return
	}

	// A platform endpoint is verified and decoded by the code that already
	// knows that vendor, rather than a second implementation here that would
	// drift from it. The normalised event is the same one the built-in tracker
	// endpoints publish, so a recipe written for "an issue changed" works
	// whichever way the delivery arrived.
	if ep.Platform != "" {
		d.servePlatform(w, r, ep, body)
		return
	}

	if reason := verify(ep, r, body); reason != "" {
		d.record(ep, r, "rejected", reason, body)
		d.log.Warn("webhook delivery rejected",
			zap.String("slug", slug), zap.String("reason", reason))
		http.Error(w, "unverified", http.StatusUnauthorized)
		return
	}

	payload := decode(body)
	payload["_webhook"] = ep.Slug
	payload["_received_at"] = time.Now().UTC().Format(time.RFC3339)
	if src := source(r); src != "" {
		payload["_source"] = src
	}

	// Published as the operator's chosen kind, which is the point: the recipe
	// subscriber matches on kind and fires the workflow, and no agent is woken
	// to read a payload a workflow can handle on its own.
	if err := d.bus.Publish(bus.NewEvent(bus.EventKind(ep.EventKind), ep.AgentID, payload)); err != nil {
		d.record(ep, r, "error", "could not publish the event: "+err.Error(), body)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	// An agent only receives kinds that were routed to inboxes at startup, and
	// a kind invented in the console at 3pm was not among them. So when — and
	// only when — the operator names an agent, the same payload is published
	// again as a user-defined event, which IS routed.
	//
	// Two events rather than one, deliberately: the first is what recipes
	// trigger on, and collapsing them would mean either recipes stop matching
	// the kind the operator chose, or agents never hear about it at all.
	if strings.TrimSpace(ep.AgentID) != "" {
		agentPayload := map[string]any{}
		for k, v := range payload {
			agentPayload[k] = v
		}
		agentPayload["event_kind"] = ep.EventKind
		if err := d.bus.Publish(bus.NewEvent(bus.EventUserDefined, ep.AgentID, agentPayload)); err != nil {
			d.log.Warn("webhook event published but not delivered to the agent",
				zap.String("slug", ep.Slug), zap.String("agent", ep.AgentID), zap.Error(err))
		}
	}

	d.record(ep, r, "accepted", "published "+ep.EventKind, body)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "event": ep.EventKind})
}

// verify checks a delivery, returning why it was refused or "" if it is good.
func verify(ep *store.WebhookEndpoint, r *http.Request, body []byte) string {
	if ep.Secret == "" {
		// No secret configured means anybody who knows the URL can post. That
		// is a deliberate choice an operator can make for a harmless event, and
		// the console says so plainly rather than pretending otherwise.
		return ""
	}

	header := strings.TrimSpace(ep.SignatureHeader)
	if header == "" {
		// Shared-token mode: the secret is sent as-is.
		supplied := r.Header.Get("X-Webhook-Token")
		if supplied == "" {
			supplied = strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		}
		if supplied == "" {
			supplied = r.URL.Query().Get("token")
		}
		if !hmac.Equal([]byte(strings.TrimSpace(supplied)), []byte(ep.Secret)) {
			return "the shared token was missing or wrong"
		}
		return ""
	}

	supplied := strings.TrimSpace(r.Header.Get(header))
	if supplied == "" {
		return "the " + header + " header was missing"
	}
	if !signatureMatches(ep.Secret, supplied, body) {
		return "the signature in " + header + " did not match"
	}
	return ""
}

// signatureMatches compares an HMAC in any of the shapes senders use.
//
// GitHub sends "sha256=<hex>", older integrations "sha1=<hex>", and plenty of
// smaller services send bare hex. Accepting all three costs nothing and saves
// an operator working out which dialect their vendor speaks.
func signatureMatches(secret, supplied string, body []byte) bool {
	algo, value := "sha256", supplied
	if i := strings.IndexByte(supplied, '='); i > 0 {
		algo, value = strings.ToLower(supplied[:i]), supplied[i+1:]
	}

	var sum []byte
	switch algo {
	case "sha1":
		m := hmac.New(sha1.New, []byte(secret))
		m.Write(body)
		sum = m.Sum(nil)
	default:
		m := hmac.New(sha256.New, []byte(secret))
		m.Write(body)
		sum = m.Sum(nil)
	}

	// hmac.Equal, not ==: a byte-by-byte comparison that stops at the first
	// difference leaks the signature one character at a time.
	return hmac.Equal([]byte(strings.ToLower(strings.TrimSpace(value))), []byte(hex.EncodeToString(sum)))
}

// decode turns a body into an event payload.
//
// JSON becomes fields a recipe can address by name. Anything else is carried as
// raw text rather than refused: a webhook that only accepts JSON is a webhook
// that rejects half of what real services send.
func decode(body []byte) map[string]any {
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err == nil && obj != nil {
		return obj
	}
	var arr []any
	if err := json.Unmarshal(body, &arr); err == nil {
		return map[string]any{"items": arr}
	}
	return map[string]any{"raw": string(body)}
}

// source names who sent a delivery, for the history view.
func source(r *http.Request) string {
	for _, h := range []string{"X-GitHub-Event", "X-Gitlab-Event", "X-Event-Key", "User-Agent"} {
		if v := strings.TrimSpace(r.Header.Get(h)); v != "" {
			return truncate(v, 80)
		}
	}
	return ""
}

func (d *CustomDispatcher) record(ep *store.WebhookEndpoint, r *http.Request, status, detail string, body []byte) {
	if err := d.store.RecordWebhookDelivery(store.WebhookDelivery{
		Endpoint: ep.Slug, Source: source(r), Status: status,
		Detail: detail, BodySample: string(body),
	}); err != nil {
		d.log.Warn("could not record a webhook delivery", zap.Error(err))
	}
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// servePlatform handles a delivery from a service KARMAX can decode.
func (d *CustomDispatcher) servePlatform(w http.ResponseWriter, r *http.Request, ep *store.WebhookEndpoint, body []byte) {
	h := tracker.New(tracker.Config{
		Source:  tracker.Source(ep.Platform),
		Secret:  ep.Secret,
		AgentID: ep.AgentID,
	}, d.bus, d.log)

	// The body was already consumed to record it, so hand the handler a fresh
	// reader over the same bytes rather than an exhausted one.
	r.Body = io.NopCloser(bytes.NewReader(body))

	rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
	h.ServeHTTP(rec, r)

	switch {
	case rec.status >= 200 && rec.status < 300:
		d.record(ep, r, "accepted", "published "+string(tracker.EventKind), body)
	case rec.status == http.StatusUnauthorized || rec.status == http.StatusForbidden:
		// Recorded as a rejection with the platform named: "the secret is
		// wrong" and "the sender is not who you think" are the same status and
		// very different problems.
		d.record(ep, r, "rejected", ep.Platform+" rejected the delivery as unverified", body)
	default:
		d.record(ep, r, "error", ep.Platform+" handler returned "+itoa(rec.status), body)
	}
}

// statusRecorder remembers what a delegated handler answered, so the delivery
// can be recorded without the handler knowing anything about this table.
type statusRecorder struct {
	http.ResponseWriter
	status  int
	written bool
}

func (s *statusRecorder) WriteHeader(code int) {
	if !s.written {
		s.status, s.written = code, true
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	s.written = true
	return s.ResponseWriter.Write(b)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	if neg {
		return "-" + string(b)
	}
	return string(b)
}
