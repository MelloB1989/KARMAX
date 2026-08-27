// Package connectors mounts connectorkit integrations into KARMAX: their tools
// onto the agent, their webhooks onto the webhook server, their pollers onto a
// ticker, and all of it behind the Broker.
package connectors

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/MelloB1989/karmax/internal/broker"
	"github.com/MelloB1989/karmax/internal/bus"
	"github.com/MelloB1989/karmax/internal/safety"
	"github.com/MelloB1989/karmax/internal/store"
	"github.com/MelloB1989/karmax/internal/tools"
	"github.com/MelloB1989/karmax/pkg/connectorkit"
	"go.uber.org/zap"
)

// maxDeliveryBytes caps a webhook body before parsing.
const maxDeliveryBytes = 4 << 20

type Host struct {
	store  *store.Store
	bus    *bus.Log
	broker *broker.Broker
	log    *zap.Logger

	registry map[string]connectorkit.Connector

	// refresh renews an expiring OAuth token before it is used. Nil until the
	// runtime supplies one.
	refresh     Refresher
	refreshUser UserRefresher

	// unconditional are connectors whose tools exist without credentials.
	unconditional map[string]bool
}

func NewHost(s *store.Store, b *bus.Log, brk *broker.Broker, log *zap.Logger) *Host {
	return &Host{store: s, bus: b, broker: brk, log: log,
		registry: map[string]connectorkit.Connector{}}
}

// Register makes a connector available to be configured. It does nothing until
// the operator supplies credentials and enables it.
func (h *Host) Register(c connectorkit.Connector) {
	h.registry[c.Manifest().ID] = c
}

// Available lists every registered connector.
func (h *Host) Available() []connectorkit.Manifest {
	out := make([]connectorkit.Manifest, 0, len(h.registry))
	for _, c := range h.registry {
		out = append(out, c.Manifest())
	}
	return out
}

// Get returns a registered connector.
func (h *Host) Get(id string) (connectorkit.Connector, bool) {
	c, ok := h.registry[id]
	return c, ok
}

// Enabled returns the connectors the operator has configured and turned on.
func (h *Host) Enabled() []connectorkit.Connector {
	creds, err := h.store.EnabledConnectors()
	if err != nil {
		h.log.Warn("could not read enabled connectors", zap.Error(err))
		return nil
	}
	var out []connectorkit.Connector
	for _, cr := range creds {
		if c, ok := h.registry[cr.Connector]; ok {
			out = append(out, c)
		}
	}
	return out
}

// credentials assembles what a connector is handed at call time.
// Refresh renews an OAuth token that is about to expire, if anything supplied
// a way to. Set by the runtime from the integration registry.
//
// Without it a connector authenticated by browser sign-in works until its
// access token expires and then simply stops, which is at its worst here: the
// loops that use these connectors run unattended in the evening, so the failure
// surfaces as a post that never happened rather than as an error anybody sees.
type Refresher func(ctx context.Context, id string) error

// SetRefresher supplies the token refresh. Optional; without it, tokens are
// used exactly as stored.
func (h *Host) SetRefresher(r Refresher) { h.refresh = r }

// UserRefresher renews one employee's OAuth token, returning the credential to
// use. It is called before every per-user call, and is a no-op unless the token
// is close to expiring.
type UserRefresher func(ctx context.Context, connector string, c store.UserCredential) (store.UserCredential, error)

// SetUserRefresher wires per-employee token renewal.
func (h *Host) SetUserRefresher(r UserRefresher) { h.refreshUser = r }

// credentialsFor resolves the credentials one call should use.
//
// For a PerUser connector this is the ACTING EMPLOYEE's authorisation, not the
// install's. The org's OAuth app config is merged in underneath, because the
// refresh flow needs the client id and secret that the employee's token was
// issued against.
//
// It refuses rather than falls back. If nobody is being acted for, or that
// person has not connected their account, there is no safe default: reading
// whichever mailbox happens to be stored would answer a question about one
// person's private data with another person's private data.
func (h *Host) credentialsFor(ctx context.Context, id string) (connectorkit.Credentials, error) {
	base, err := h.credentials(id)
	if err != nil {
		return connectorkit.Credentials{}, err
	}

	c, ok := h.Get(id)
	if !ok || !c.Manifest().PerUser {
		return base, nil
	}

	member := connectorkit.ActorFrom(ctx)
	if member == "" {
		return connectorkit.Credentials{}, fmt.Errorf(
			"%s acts as an individual person, and this work is not on anyone's behalf — "+
				"it cannot run from a scheduled loop or a webhook without an acting member", id)
	}

	uc, err := h.store.UserCredential(id, member)
	if err != nil {
		return connectorkit.Credentials{}, err
	}
	if uc == nil {
		return connectorkit.Credentials{}, fmt.Errorf(
			"%s has not been connected by %s — they need to connect their account in the "+
				"console before this can act for them", id, member)
	}

	if h.refreshUser != nil {
		refreshed, rerr := h.refreshUser(ctx, id, *uc)
		if rerr != nil {
			return connectorkit.Credentials{}, fmt.Errorf(
				"%s's sign-in for %s could not be refreshed: %w", member, id, rerr)
		}
		uc = &refreshed
	}

	// The employee's token wins; the org app's config underneath it is what the
	// connector needs to talk to the provider at all.
	return connectorkit.Credentials{
		Config:      base.Config,
		AccessToken: uc.AccessToken,
		ExpiresAt:   derefTime(uc.ExpiresAt),
		Member:      uc.Member,
		Account:     uc.Account,
	}, nil
}

func derefTime(t *time.Time) time.Time {
	if t == nil {
		return time.Time{}
	}
	return *t
}

func (h *Host) credentials(id string) (connectorkit.Credentials, error) {
	// Before reading, not after failing. The refresh is a no-op unless this is
	// an OAuth connector whose token is within five minutes of expiring.
	if h.refresh != nil {
		if err := h.refresh(context.Background(), id); err != nil {
			h.log.Warn("could not refresh a connector's sign-in",
				zap.String("connector", id), zap.Error(err))
		}
	}

	rec, err := h.store.Credential(id)
	if err != nil {
		return connectorkit.Credentials{}, fmt.Errorf("connector %s is not configured: %w", id, err)
	}
	// An unconditional connector may run with nothing stored. What it can
	// actually do without credentials is its own business — it is the one that
	// knows whether this particular call needs them.
	if rec == nil {
		if h.unconditional[id] {
			return connectorkit.Credentials{}, nil
		}
		return connectorkit.Credentials{}, fmt.Errorf("connector %s is not configured", id)
	}
	if !rec.Enabled && !h.unconditional[id] {
		return connectorkit.Credentials{}, fmt.Errorf("connector %s is configured but not enabled", id)
	}
	cr := connectorkit.Credentials{Config: rec.Config, AccessToken: rec.AccessToken}
	if rec.ExpiresAt != nil {
		cr.ExpiresAt = *rec.ExpiresAt
	}
	return cr, nil
}

// RegisterUnconditional registers a connector whose tools exist whether or not
// it has credentials.
//
// For connectors with a useful answer when unconfigured. The social ones have
// one: with dry run on they deliver a draft to the operator and never touch the
// platform, so requiring an X account before you can preview what KARMAX would
// say gets it exactly backwards — previewing is what you do BEFORE connecting.
//
// Not a way around configuration. A real post still needs real credentials, and
// the connector says so plainly when it lacks them.
func (h *Host) RegisterUnconditional(c connectorkit.Connector) {
	h.Register(c)
	if h.unconditional == nil {
		h.unconditional = map[string]bool{}
	}
	id := c.Manifest().ID
	h.unconditional[id] = true
	// Grants normally land when the operator enables a connector. A connector
	// that works without being enabled has to be granted here instead, or its
	// tools resolve and are then refused by the Broker — which reads as a bug in
	// the sandbox rather than as a missing grant.
	if err := h.GrantFromManifest(id); err != nil {
		h.log.Error("could not grant an unconditional connector its own tools",
			zap.String("connector", id), zap.Error(err))
	}
}

// Tools adapts every enabled connector's tools into KARMAX tools.
func (h *Host) Tools() []tools.Tool {
	seen := map[string]bool{}
	var out []tools.Tool
	add := func(c connectorkit.Connector) {
		id := c.Manifest().ID
		if seen[id] {
			return
		}
		seen[id] = true
		for _, t := range c.Tools() {
			out = append(out, &connectorTool{host: h, connector: id, tool: t})
		}
	}
	for _, c := range h.Enabled() {
		add(c)
	}
	for id := range h.unconditional {
		if c, ok := h.Get(id); ok {
			add(c)
		}
	}
	return out
}

// connectorTool makes a connector's tool indistinguishable from a built-in one,
// which is what stops connector tools becoming a second-class tier.
type connectorTool struct {
	host      *Host
	connector string
	tool      connectorkit.Tool
}

func (t *connectorTool) Manifest() tools.ToolManifest {
	return tools.ToolManifest{
		Name:        t.tool.Name,
		Description: t.tool.Description,
		Parameters:  t.tool.Parameters,
	}
}

func (t *connectorTool) Execute(ctx context.Context, input map[string]any) (tools.ToolResult, error) {
	subject := broker.ConnectorSubject(t.connector)
	if err := t.host.broker.For(subject).Tool(t.tool.Name); err != nil {
		return tools.ErrorResult(err), nil
	}
	cr, err := t.host.credentialsFor(ctx, t.connector)
	if err != nil {
		return tools.ErrorResult(err), nil
	}
	out, err := t.tool.Call(ctx, cr, input)
	if err != nil {
		return tools.ErrorResult(err), nil
	}
	return tools.SuccessResult(out), nil
}

// MountWebhooks registers each enabled connector's webhook routes.
// WebhookPrefix is where connector webhooks are exposed publicly.
//
// Chosen because nothing else claims it: the console SPA owns /connectors/:id,
// the console API owns /api/console/*, and a CDN in front of both has to be
// able to send a webhook to the daemon without ambiguity.
const WebhookPrefix = "/hooks"

func (h *Host) MountWebhooks(add func(pattern string, handler http.HandlerFunc)) {
	for _, c := range h.Enabled() {
		id := c.Manifest().ID
		for _, src := range c.Sources() {
			if src.Kind != connectorkit.SourceWebhook || src.Path == "" {
				continue
			}
			handler := h.deliveryHandler(id, src)
			add(src.Path, handler)

			// Also mount under /hooks, which is the address an operator is
			// given. A connector's own path is chosen for readability —
			// GitHub's is /connectors/github — and that collides head-on with
			// the console SPA, which already owns /connectors/:id as a PAGE. A
			// CDN routing rule cannot tell a browser opening that page from
			// GitHub POSTing to it, so one of the two would break.
			//
			// /hooks/* belongs to nothing else, which is what lets a single
			// HTTPS front door serve both. Both paths stay live: an install
			// that already told GitHub the unprefixed address keeps working.
			add(WebhookPrefix+src.Path, handler)

			h.log.Info("connector webhook mounted",
				zap.String("connector", id), zap.String("path", src.Path),
				zap.String("public_path", WebhookPrefix+src.Path))
		}
	}
}

func (h *Host) deliveryHandler(id string, src connectorkit.EventSource) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST required", http.StatusMethodNotAllowed)
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, maxDeliveryBytes))
		if err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		cr, err := h.credentials(id)
		if err != nil {
			http.Error(w, "not configured", http.StatusServiceUnavailable)
			return
		}

		headers := map[string]string{}
		for k, v := range r.Header {
			if len(v) > 0 {
				headers[k] = v[0]
			}
		}
		// Verified before parsing: an unverified delivery is untrusted input
		// that would otherwise reach a decoder and then the event log.
		if src.Verify != nil && !src.Verify(cr, headers, body) {
			h.log.Warn("connector delivery failed verification",
				zap.String("connector", id), zap.String("path", src.Path))
			http.Error(w, "unverified", http.StatusUnauthorized)
			return
		}

		payloads, err := src.Decode(headers, body)
		if err != nil {
			h.log.Warn("connector delivery could not be decoded",
				zap.String("connector", id), zap.Error(err))
			http.Error(w, "undecodable", http.StatusBadRequest)
			return
		}
		for _, p := range payloads {
			if err := h.publish(id, src, p); err != nil {
				http.Error(w, "could not record the event", http.StatusInternalServerError)
				return
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "ok", "events": len(payloads)})
	}
}

// publish records one connector event, fencing the free-text fields on the way.
//
// Anything a stranger typed — an issue body, a comment, a commit message —
// reaches a prompt eventually, so it is marked as data here rather than at
// every place that later reads it.
func (h *Host) publish(id string, src connectorkit.EventSource, payload map[string]any) error {
	fenced := make(map[string]any, len(payload)+2)
	source := fmt.Sprintf("%s (%v)", id, payload["url"])
	for k, v := range payload {
		if s, ok := v.(string); ok && isFreeText(k) && strings.TrimSpace(s) != "" {
			fenced[k] = safety.Fence(source, s)
			continue
		}
		fenced[k] = v
	}
	fenced["connector"] = id
	fenced["source"] = src.ID

	// A source may carry several event kinds down one webhook. GitHub delivers
	// every event type to a single URL, so the connector cannot mount a path
	// per kind; it names the kind in the payload instead, and that name wins.
	// Without this a recipe could only ever await "github.event" and match on a
	// field, which is a worse thing to write and a worse thing to read.
	kind := src.EventKind
	if k, ok := payload["kind"].(string); ok && strings.TrimSpace(k) != "" {
		kind = k
	}

	if err := h.bus.Publish(bus.NewEvent(bus.EventKind(kind), "", fenced)); err != nil {
		h.log.Error("connector event could not be recorded",
			zap.String("connector", id), zap.Error(err))
		return err
	}
	return nil
}

// isFreeText names the fields a stranger controls.
func isFreeText(key string) bool {
	switch key {
	case "body", "title", "text", "message", "comment", "description", "summary":
		return true
	}
	return false
}

// StartPollers runs the polling sources of enabled connectors.
func (h *Host) StartPollers(ctx context.Context) {
	for _, c := range h.Enabled() {
		id := c.Manifest().ID
		for _, src := range c.Sources() {
			if src.Kind != connectorkit.SourcePoll || src.Poll == nil {
				continue
			}
			go h.poll(ctx, id, src)
		}
	}
}

func (h *Host) poll(ctx context.Context, id string, src connectorkit.EventSource) {
	interval := src.Interval
	if interval < time.Minute {
		interval = time.Minute
	}
	t := time.NewTicker(interval)
	defer t.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		cr, err := h.credentials(id)
		if err != nil {
			continue // disabled or unconfigured since the poller started
		}
		cursor, err := h.store.SourceCursor(id, src.ID)
		if err != nil {
			h.log.Warn("could not read a poll cursor", zap.String("connector", id), zap.Error(err))
			continue
		}
		events, next, err := src.Poll(ctx, cr, cursor)
		if err != nil {
			h.log.Warn("connector poll failed", zap.String("connector", id), zap.Error(err))
			continue
		}
		published := true
		for _, p := range events {
			if err := h.publish(id, src, p); err != nil {
				published = false
				break
			}
		}
		// The cursor advances only when everything it covers is durable, so a
		// failure re-delivers rather than skipping.
		if published && next != "" && next != cursor {
			if err := h.store.SetSourceCursor(id, src.ID, next); err != nil {
				h.log.Warn("could not record a poll cursor", zap.String("connector", id), zap.Error(err))
			}
		}
	}
}

// GrantFromManifest records the capabilities a connector declared, so enabling
// it grants exactly what its manifest asked for and nothing else.
func (h *Host) GrantFromManifest(id string) error {
	c, ok := h.registry[id]
	if !ok {
		return fmt.Errorf("no connector %q", id)
	}
	subject := broker.ConnectorSubject(id)
	if err := h.broker.RevokeAll(subject); err != nil {
		return err
	}
	for _, capability := range c.Manifest().Capabilities {
		class, value, ok := strings.Cut(capability, ":")
		if !ok {
			return fmt.Errorf("connector %s declared %q, which is not <capability>:<value>", id, capability)
		}
		if err := h.broker.Grant(store.Grant{
			Subject: subject, Capability: class, Value: value, GrantedBy: "connector-manifest",
		}); err != nil {
			return err
		}
	}
	// Its own tools, which it obviously needs and the operator did not have to
	// enumerate.
	for _, t := range c.Tools() {
		if err := h.broker.Grant(store.Grant{
			Subject: subject, Capability: store.CapTool, Value: t.Name, GrantedBy: "connector-manifest",
		}); err != nil {
			return err
		}
	}
	h.broker.SetTrust(subject, broker.Registry)
	return nil
}
