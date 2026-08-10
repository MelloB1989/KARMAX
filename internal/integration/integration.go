// Package integration is the one place KARMAX knows what it is connected to.
//
// There were two systems and neither could authenticate itself. Comms channels
// (WhatsApp, Slack, Discord, Telegram) took a token out of karmax.yaml and had
// no health check, no login and no story for OAuth. Connectors modelled auth
// properly — AuthMethod covers key, OAuth and CLI-session, and the credential
// store already persists tokens — but nothing outside connectors used any of
// it, so every new integration was going to invent its own way to hold a
// secret.
//
// This is that model, moved somewhere both halves can reach. An integration
// declares what it needs and how to check itself; KARMAX owns where the secret
// lives, how it is obtained, and whether it currently works.
package integration

import (
	"context"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/MelloB1989/karmax/pkg/connectorkit"
)

// Kind says what an integration is for. Both kinds authenticate identically;
// this only decides where it shows up.
type Kind string

const (
	// KindChannel carries conversation — the operator talks through it.
	KindChannel Kind = "channel"
	// KindConnector exposes tools — the agent acts through it.
	KindConnector Kind = "connector"
)

// Manifest is what an operator reads before connecting something.
type Manifest struct {
	ID          string
	Name        string
	Description string
	Kind        Kind
	// Config declares what must be supplied, so setup can be checked before
	// anything runs rather than failing on the first call.
	Config []connectorkit.ConfigField
	// SetupURL is where to go to get the credential — the Slack app page, the
	// Notion integrations page. Printed by `karmax login`, because "paste your
	// API key" is not help if you do not know where they live.
	SetupURL string
	// Account distinguishes several logins to the same provider, e.g. two
	// GitHub accounts. Empty is the primary.
	Account string
}

// Integration is anything KARMAX authenticates and depends on.
type Integration interface {
	Manifest() Manifest
	// Auth says how credentials are obtained. KARMAX owns storage and refresh;
	// an integration never persists a token itself.
	Auth() connectorkit.AuthMethod
	// Health reports whether it currently works. Called with whatever
	// credentials resolved, including none.
	Health(ctx context.Context, c connectorkit.Credentials) error
}

// Status is the last thing we learned about an integration.
type Status struct {
	ID      string
	Name    string
	Kind    Kind
	Account string
	// Configured is whether a credential resolved at all.
	Configured bool
	// Healthy is the result of the last check. Meaningless when Checked is zero.
	Healthy bool
	Error   string
	Checked time.Time
	// Source says where the credential came from, so "I set it in the yaml but
	// it is using something else" is answerable.
	Source   string
	AuthKind connectorkit.AuthKind
}

// Registry holds every integration this instance knows about.
type Registry struct {
	mu     sync.RWMutex
	byID   map[string]Integration
	order  []string
	status map[string]Status
	creds  *Resolver
}

// NewRegistry builds a registry over a credential resolver.
func NewRegistry(r *Resolver) *Registry {
	return &Registry{
		byID:   map[string]Integration{},
		status: map[string]Status{},
		creds:  r,
	}
}

// Register adds an integration. Registering twice replaces, so a channel and a
// connector cannot both claim an id and silently disagree.
func (r *Registry) Register(in Integration) {
	id := in.Manifest().ID
	if strings.TrimSpace(id) == "" {
		return
	}
	r.mu.Lock()
	if _, seen := r.byID[id]; !seen {
		r.order = append(r.order, id)
	}
	r.byID[id] = in
	r.mu.Unlock()

	// Declared as it registers, so the environment fallback knows which
	// variable names belong to it without anybody having to remember.
	r.Declare(in)
}

// Get returns one integration.
func (r *Registry) Get(id string) (Integration, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	in, ok := r.byID[id]
	return in, ok
}

// List returns every registered integration, in registration order.
func (r *Registry) List() []Integration {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Integration, 0, len(r.order))
	for _, id := range r.order {
		out = append(out, r.byID[id])
	}
	return out
}

// Credentials resolves what an integration should be called with.
func (r *Registry) Credentials(id string) (connectorkit.Credentials, string, error) {
	return r.creds.Resolve(id)
}

// Check runs one integration's health check and records the result.
func (r *Registry) Check(ctx context.Context, id string) Status {
	in, ok := r.Get(id)
	if !ok {
		return Status{ID: id, Error: "not registered", Checked: time.Now()}
	}
	m := in.Manifest()
	st := Status{
		ID: m.ID, Name: m.Name, Kind: m.Kind, Account: m.Account,
		Checked: time.Now(), AuthKind: in.Auth().Kind,
	}

	creds, source, err := r.creds.Resolve(id)
	st.Source = source
	if err != nil {
		// Surfaced rather than folded into "not configured". A store that
		// cannot be read looks identical to a credential nobody set, and the
		// two need completely different things done about them.
		st.Error = err.Error()
		st.Checked = time.Now()
		r.mu.Lock()
		r.status[id] = st
		r.mu.Unlock()
		return st
	}
	st.Configured = credentialPresent(in.Auth(), creds)

	// Checked even when nothing resolved: an AuthNone integration is healthy
	// with no credential, and a CLI one reports on a session KARMAX does not
	// hold. "Not configured" is a conclusion the check reaches, not a reason to
	// skip it.
	if herr := in.Health(ctx, creds); herr != nil {
		st.Healthy, st.Error = false, herr.Error()
	} else {
		st.Healthy = true
	}

	// For a CLI session the health check IS the credential check — KARMAX holds
	// nothing, so "is it configured" and "is it signed in" are one question.
	if in.Auth().Kind == connectorkit.AuthCLI {
		st.Configured = st.Healthy
		if st.Healthy {
			st.Source = "its own session"
		}
	}

	r.mu.Lock()
	r.status[id] = st
	r.mu.Unlock()
	return st
}

// CheckAll runs every health check and returns the results, id-sorted.
func (r *Registry) CheckAll(ctx context.Context) []Status {
	var out []Status
	for _, in := range r.List() {
		out = append(out, r.Check(ctx, in.Manifest().ID))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Statuses returns the last known state without re-checking, for anything on a
// request path that must not wait on a network call.
func (r *Registry) Statuses() []Status {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Status, 0, len(r.status))
	for _, s := range r.status {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// credentialPresent reports whether the resolved credentials satisfy the auth
// method — which is a different question from whether they work.
func credentialPresent(a connectorkit.AuthMethod, c connectorkit.Credentials) bool {
	switch a.Kind {
	case connectorkit.AuthNone:
		return true
	case connectorkit.AuthAPIKey:
		return strings.TrimSpace(c.Get(a.APIKeyField)) != ""
	case connectorkit.AuthOAuth2:
		return strings.TrimSpace(c.AccessToken) != ""
	case connectorkit.AuthCLI:
		// The session belongs to the binary, not to KARMAX, so presence is
		// whatever Health says.
		return true
	}
	return false
}

// Forget drops an integration's stored credentials, handing control back to
// karmax.yaml and the environment.
func (r *Registry) Forget(id string) error { return r.creds.Forget(id) }

// Declare records an integration's config fields with the resolver, so the
// environment fallback knows which names to look for.
func (r *Registry) Declare(in Integration) {
	m := in.Manifest()
	r.creds.Declare(m.ID, m.Config)
}
