package api

import (
	"context"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/MelloB1989/karmax/internal/connectors"
	"github.com/MelloB1989/karmax/internal/store"
	"github.com/MelloB1989/karmax/pkg/connectorkit"
	"go.uber.org/zap"
)

// Connectors: what is wired up, and what an operator must do to wire up more.
//
// Setup is instruction text plus this server's own callback URLs. No
// third-party call happens until credentials are submitted and a health check
// is explicitly run — a wizard that starts hitting APIs while you read it is a
// wizard that leaks a half-typed token.

type connectorSummary struct {
	ID            string  `json:"id"`
	Name          string  `json:"name"`
	Kind          string  `json:"kind"`
	Status        string  `json:"status"`
	Detail        string  `json:"detail"`
	LastCheckedAt *string `json:"last_checked_at"`
}

type setupStep struct {
	Title string `json:"title"`
	Body  string `json:"body"`
	Value string `json:"value,omitempty"`
	URL   string `json:"url,omitempty"`
	Done  *bool  `json:"done,omitempty"`
}

type setupField struct {
	Key         string `json:"key"`
	Label       string `json:"label"`
	Type        string `json:"type"`
	Placeholder string `json:"placeholder,omitempty"`
	Required    bool   `json:"required"`
}

// Health verdicts come from the store, written by the connector host's
// background prober. They used to live in a map here, which meant they started
// empty on every boot and only ever filled if a human clicked — so every
// connector read "degraded, not checked yet" including the ones working
// perfectly, and the next restart threw the answer away again.

// healthStale is how old a verdict may be before the console stops presenting
// it as current. Comfortably longer than the probe interval, so an ordinary
// gap between sweeps does not make everything look unknown.
const healthStale = 3 * connectors.ProbeInterval

// summariseConnector reports a connector's state without calling anyone.
//
// A list view must not health-check: three vendors' round trips on every page
// load would rate-limit the install and make the page as slow as the slowest of
// them. It reports the last verdict the prober recorded.
func (s *ConsoleServer) summariseConnector(m connectorkit.Manifest, health map[string]store.ConnectorHealth) connectorSummary {
	sum := connectorSummary{ID: m.ID, Name: m.Name, Kind: m.ID, Status: "not_configured"}

	if h, ok := health[m.ID]; ok && h.Status != "" {
		sum.Status, sum.Detail = h.Status, h.Detail
		sum.LastCheckedAt = rfc3339Ptr(h.CheckedAt)
		if h.Stale(healthStale) {
			// Say it is old rather than quietly presenting a stale verdict as
			// current — the difference matters when someone is deciding
			// whether an integration is the cause of a problem.
			sum.Detail = h.Detail + " (last checked a while ago)"
		}
		return sum
	}

	// Never checked. Report what is knowable without a network call.
	cred, err := s.store.Credential(m.ID)
	if err != nil || cred == nil {
		if self, ok := s.connectorByID(m.ID); ok {
			if connectors.SelfConfigured(self, connectorkit.Credentials{Config: map[string]string{}}) {
				sum.Status = "degraded"
				sum.Detail = "Configured outside the console; checking shortly"
				return sum
			}
		}
		sum.Detail = "No credentials saved yet"
		return sum
	}
	sum.Status = "degraded"
	sum.Detail = "Credentials saved; checking shortly"
	sum.LastCheckedAt = rfc3339Ptr(cred.UpdatedAt)
	return sum
}

func (s *ConsoleServer) handleConnectors(w http.ResponseWriter, r *http.Request) {
	out := []connectorSummary{}
	if s.conns != nil {
		// One query for every verdict, rather than one per connector.
		health, err := s.store.AllConnectorHealth()
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		for _, m := range s.conns.Available() {
			out = append(out, s.summariseConnector(m, health))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	writeJSON(w, http.StatusOK, map[string]any{"connectors": out})
}

// publicBase is the address operators actually reach this server at.
//
// Configured explicitly rather than inferred: behind a CDN or a reverse proxy
// the server cannot know its own public name, and a setup wizard that prints
// an unreachable callback URL is worse than one that prints none.
func (s *ConsoleServer) publicBase() string {
	if s.cfg != nil {
		if u := strings.TrimRight(strings.TrimSpace(s.cfg.Console.PublicURL), "/"); u != "" {
			return u
		}
	}
	return ""
}

// callbackURL is the address a third party should deliver webhooks to.
//
// The PATH comes from the connector itself, via the EventSource it declares.
// It used to be built as "/hooks/" + id, which was invented: GitHub's real path
// is /connectors/github and Jira's is a set of three. So the wizard printed a
// URL that had never existed, and pasting it into GitHub produced deliveries
// that 404'd — worse than showing nothing, because it looked done.
//
// The HOST comes from webhooks.public_url, falling back to console.public_url.
// They are usually different addresses: a browser destination and a machine
// destination, on this deployment not even the same port.
func (s *ConsoleServer) callbackURL(connectorID string) string {
	base := s.webhookBase()
	if base == "" {
		return ""
	}
	c, ok := s.connectorByID(connectorID)
	if !ok {
		return ""
	}
	for _, src := range c.Sources() {
		if src.Kind == connectorkit.SourceWebhook && src.Path != "" {
			// The /hooks-prefixed form, not the connector's own path: that one
			// collides with the console SPA, so it cannot be served from the
			// same HTTPS front door. Both are mounted; this is the one to hand
			// out.
			return base + connectors.WebhookPrefix + src.Path
		}
	}
	// A connector with no webhook source has no callback URL, and saying so by
	// omission beats inventing one.
	return ""
}

// webhookBase is where this install's webhook server is reachable from outside.
func (s *ConsoleServer) webhookBase() string {
	if s.cfg != nil {
		if u := strings.TrimRight(strings.TrimSpace(s.cfg.Webhooks.PublicURL), "/"); u != "" {
			return u
		}
	}
	return s.publicBase()
}

func (s *ConsoleServer) handleConnectorSetup(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if s.conns == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "no such connector"})
		return
	}
	c, ok := s.conns.Get(id)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "no such connector"})
		return
	}
	m := c.Manifest()

	// Fields come from the connector's own manifest rather than a table in the
	// console, so a connector that grows a config key does not need a matching
	// frontend change to become configurable.
	fields := make([]setupField, 0, len(m.Config))
	for _, f := range m.Config {
		typ := "text"
		if f.Secret {
			typ = "secret"
		} else if strings.Contains(strings.ToLower(f.Key), "url") {
			typ = "url"
		}
		fields = append(fields, setupField{
			Key: f.Key, Label: labelFor(f.Key), Type: typ,
			Placeholder: f.Default, Required: f.Required,
		})
	}

	callback := s.callbackURL(id)

	// A connector that knows its own setup says so itself. GitHub's guide has a
	// step with no field attached — install the App on your repositories —
	// which a guide generated from the config list can never mention, and which
	// is exactly the step people miss.
	var steps []setupStep
	if guide, ok := c.(connectorkit.SetupGuide); ok {
		cred, _ := s.store.Credential(id)
		known := connectorkit.Credentials{Config: map[string]string{}}
		if cred != nil {
			known.Config, known.AccessToken = cred.Config, cred.AccessToken
		}
		for _, st := range guide.SetupSteps(known, callback) {
			steps = append(steps, setupStep{Title: st.Title, Body: st.Body, Value: st.Value, URL: st.URL, Done: st.Done})
		}
	}

	// A webhook URL that is not HTTPS is worth saying out loud at the moment
	// someone is about to paste it into a third party, rather than leaving them
	// to notice the scheme.
	if callback != "" && !strings.HasPrefix(callback, "https://") {
		steps = append(steps, setupStep{
			Title: "This callback URL is not HTTPS",
			Body: "Deliveries to it cross the network in the clear. The signature still proves " +
				"they are genuine and unmodified, but anyone in between can read the payload. " +
				"Put a certificate in front of the webhook server when you can.",
			Value: callback,
		})
	}

	if len(steps) == 0 {
		steps = genericSteps(m, callback)
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"id": id, "steps": steps, "fields": fields, "callback_url": callback,
	})
}

// labelFor turns a config key into something readable: "client_secret" reads
// as "Client secret".
func labelFor(key string) string {
	words := strings.FieldsFunc(key, func(r rune) bool { return r == '_' || r == '-' || r == '.' })
	if len(words) == 0 {
		return key
	}
	words[0] = strings.ToUpper(words[0][:1]) + words[0][1:]
	return strings.Join(words, " ")
}

func (s *ConsoleServer) handleConnectorCredentials(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if s.conns == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "no such connector"})
		return
	}
	c, ok := s.conns.Get(id)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "no such connector"})
		return
	}

	var body map[string]string
	if err := readJSON(r, &body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid JSON"})
		return
	}

	m := c.Manifest()
	var missing []string
	for _, f := range m.Config {
		if f.Required && strings.TrimSpace(body[f.Key]) == "" {
			missing = append(missing, f.Key)
		}
	}
	if len(missing) > 0 {
		writeJSON(w, http.StatusUnprocessableEntity,
			map[string]any{"error": "missing required fields: " + strings.Join(missing, ", ")})
		return
	}

	existing, _ := s.store.Credential(id)
	cred := store.Credential{Connector: id, Config: map[string]string{}, Enabled: true}
	if existing != nil {
		cred.AccessToken, cred.RefreshToken, cred.ExpiresAt = existing.AccessToken, existing.RefreshToken, existing.ExpiresAt
		for k, v := range existing.Config {
			cred.Config[k] = v
		}
	}
	for k, v := range body {
		cred.Config[k] = v
	}
	if tok := strings.TrimSpace(body["access_token"]); tok != "" {
		cred.AccessToken = tok
	}

	if err := s.store.SaveCredential(cred); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}

	// New credentials invalidate the old verdict — leaving a stale "healthy"
	// next to a freshly pasted token would be the console vouching for
	// something it has not tested. Probe immediately rather than telling the
	// operator to go and click: they just gave us everything needed to check.
	verdict := s.conns.Probe(r.Context(), id)
	s.audit(r, "human", consoleUser(r).Member, "console.connector.credentials", id, verdict.Status, "credentials updated")
	s.log.Info("connector credentials updated",
		zap.String("connector", id), zap.String("health", verdict.Status))

	writeJSON(w, http.StatusOK, s.summariseConnector(m, map[string]store.ConnectorHealth{id: verdict}))
}

func (s *ConsoleServer) handleConnectorHealthCheck(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if s.conns == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "no such connector"})
		return
	}
	c, ok := s.conns.Get(id)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "no such connector"})
		return
	}

	// A per-user connector is checked AS THE CALLER when they have connected,
	// because "does this work" has a different answer per person. Everything
	// else goes through the same prober the background sweep uses, so the
	// button and the sweep can never disagree about what healthy means.
	if c.Manifest().PerUser {
		member := consoleUser(r).Member
		uc, err := s.store.UserCredential(id, member)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		if uc != nil {
			cred, _ := s.store.Credential(id)
			known := connectorkit.Credentials{Config: map[string]string{}, AccessToken: uc.AccessToken,
				Member: uc.Member, Account: uc.Account}
			if cred != nil {
				known.Config = cred.Config
			}
			ctx, cancel := context.WithTimeout(r.Context(), 25*time.Second)
			defer cancel()

			res := store.ConnectorHealth{Connector: id, Status: "healthy",
				Detail: "Connected as " + uc.Account, CheckedAt: time.Now()}
			if err := c.Health(ctx, known); err != nil {
				res.Status, res.Detail = "failed", err.Error()
			}
			s.reportHealth(w, r, id, res)
			return
		}

		// They have not connected. Say so about THEM: the background prober
		// reports on the org ("nobody has connected yet"), which is the right
		// thing for a list but not an answer to "why does this not work for me".
		if cred, _ := s.store.Credential(id); cred != nil {
			s.reportHealth(w, r, id, store.ConnectorHealth{
				Connector: id, Status: "degraded", CheckedAt: time.Now(),
				Detail: "The app is set up, but you have not connected your own account yet",
			})
			return
		}
	}

	s.reportHealth(w, r, id, s.conns.Probe(r.Context(), id))
}

// reportHealth persists a verdict and returns it.
func (s *ConsoleServer) reportHealth(w http.ResponseWriter, r *http.Request, id string, res store.ConnectorHealth) {
	res.Connector = id
	if err := s.store.SaveConnectorHealth(res); err != nil {
		s.log.Warn("could not record a connector's health", zap.Error(err))
	}
	s.audit(r, "human", consoleUser(r).Member, "console.connector.health", id, res.Status, res.Detail)
	writeJSON(w, http.StatusOK, map[string]any{
		"status": res.Status, "detail": res.Detail, "checked_at": rfc3339(res.CheckedAt),
	})
}

// genericSteps is the guide for a connector that does not provide its own.
func genericSteps(m connectorkit.Manifest, callback string) []setupStep {
	steps := []setupStep{{Title: "Create the app", Body: m.Description}}
	if callback != "" {
		steps = append(steps, setupStep{
			Title: "Point it back here",
			Body:  "Use this as the callback / webhook URL in the app's settings.",
			Value: callback,
		})
	} else {
		steps = append(steps, setupStep{
			Title: "Set console.public_url first",
			Body: "This server does not know its own public address, so it cannot show you a " +
				"callback URL to copy. Set console.public_url in karmax.yaml and reload.",
		})
	}
	steps = append(steps, setupStep{
		Title: "Paste the credentials below",
		Body:  "They are stored by KARMAX, never by the connector, and are never shown again once saved.",
	})
	if len(m.Capabilities) > 0 {
		steps = append(steps, setupStep{
			Title: "What enabling this grants",
			Body:  strings.Join(m.Capabilities, ", "),
		})
	}
	return steps
}

// connectorByID looks a connector up, tolerating a nil host.
func (s *ConsoleServer) connectorByID(id string) (connectorkit.Connector, bool) {
	if s.conns == nil {
		return nil, false
	}
	return s.conns.Get(id)
}
