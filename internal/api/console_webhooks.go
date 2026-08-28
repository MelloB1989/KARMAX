package api

import (
	"net/http"
	"sort"
	"strings"

	"github.com/MelloB1989/karmax/internal/connectors"
	"github.com/MelloB1989/karmax/internal/store"
	"github.com/MelloB1989/karmax/internal/webhook"
	"github.com/MelloB1989/karmax/pkg/connectorkit"
)

// Two kinds of webhook, shown together because an operator thinks of them as
// one list.
//
//   - PLATFORM: declared by a connector KARMAX already integrates with. The
//     body shape is known, so a delivery is decoded into a typed event and a
//     recipe can act on it without an agent ever reading the payload.
//   - CUSTOM: anything else. The shape is unknown, so the payload becomes the
//     event as-is under a kind the operator chooses — which a recipe can still
//     trigger on.
//
// The distinction that matters to whoever is reading the page is exactly that:
// for a platform webhook, KARMAX knows what the fields mean.

type webhookRow struct {
	ID   string `json:"id"`
	Kind string `json:"kind"` // platform | custom
	Name string `json:"name"`
	// Connector is set for a platform webhook: the integration that owns it.
	Connector string `json:"connector,omitempty"`
	Slug      string `json:"slug,omitempty"`
	URL       string `json:"url"`
	// EventKind is what a recipe triggers on.
	EventKind   string `json:"event_kind"`
	Description string `json:"description,omitempty"`
	Enabled     bool   `json:"enabled"`
	// Live says the route is actually mounted and answering. A platform
	// webhook is only mounted once its connector has credentials, so this can
	// be false for something that otherwise looks configured.
	Live bool `json:"live"`
	// Secured says a delivery has to prove itself.
	Secured         bool   `json:"secured"`
	SignatureHeader string `json:"signature_header,omitempty"`
	AgentID         string `json:"agent_id,omitempty"`
	CreatedBy       string `json:"created_by,omitempty"`
	UpdatedAt       string `json:"updated_at,omitempty"`
}

type deliveryRow struct {
	ID         string `json:"id"`
	Endpoint   string `json:"endpoint"`
	Source     string `json:"source"`
	Status     string `json:"status"`
	Detail     string `json:"detail"`
	BodySample string `json:"body_sample"`
	ReceivedAt string `json:"received_at"`
}

func (s *ConsoleServer) handleListWebhooks(w http.ResponseWriter, r *http.Request) {
	out := []webhookRow{}

	// Platform webhooks, derived from what each connector declares. Nothing is
	// stored for these: the connector is the source of truth for its own paths,
	// and a copy in the database would drift the first time one changed.
	if s.conns != nil {
		for _, m := range s.conns.Available() {
			c, ok := s.conns.Get(m.ID)
			if !ok {
				continue
			}
			configured := false
			if cred, err := s.store.Credential(m.ID); err == nil && cred != nil {
				configured = true
			} else if connectors.SelfConfigured(c, connectorkit.Credentials{Config: map[string]string{}}) {
				configured = true
			}

			for _, src := range c.Sources() {
				if src.Kind != connectorkit.SourceWebhook || src.Path == "" {
					continue
				}
				out = append(out, webhookRow{
					ID:        m.ID + ":" + src.ID,
					Kind:      "platform",
					Name:      m.Name,
					Connector: m.ID,
					URL:       s.webhookBase() + connectors.WebhookPrefix + src.Path,
					EventKind: src.EventKind,
					Description: "KARMAX knows this payload's shape, so a recipe can act on the " +
						"decoded event without an agent reading it.",
					Enabled: true,
					// Routes mount at startup for connectors that have
					// credentials. Saying so here is the difference between "it
					// is set up" and "it is answering".
					Live:    configured,
					Secured: src.Verify != nil,
				})
			}
		}
	}

	custom, err := s.store.ListWebhookEndpoints()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	for _, e := range custom {
		out = append(out, webhookRow{
			ID: e.ID, Kind: "custom", Name: e.Name, Slug: e.Slug,
			URL:         s.webhookBase() + webhook.CustomPrefix + e.Slug,
			EventKind:   e.EventKind,
			Description: e.Description,
			Enabled:     e.Enabled,
			// Custom webhooks are dispatched from a lookup, so an enabled one
			// is always answering — there is nothing to mount.
			Live:            e.Enabled,
			Secured:         e.Secret != "",
			SignatureHeader: e.SignatureHeader,
			AgentID:         e.AgentID,
			CreatedBy:       e.CreatedBy,
			UpdatedAt:       rfc3339(e.UpdatedAt),
		})
	}

	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Kind != out[j].Kind {
			return out[i].Kind == "platform"
		}
		return out[i].Name < out[j].Name
	})
	writeJSON(w, http.StatusOK, map[string]any{"webhooks": out, "base_url": s.webhookBase()})
}

func (s *ConsoleServer) handleCreateWebhook(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Slug            string `json:"slug"`
		Name            string `json:"name"`
		Description     string `json:"description"`
		EventKind       string `json:"event_kind"`
		Secret          string `json:"secret"`
		SignatureHeader string `json:"signature_header"`
		AgentID         string `json:"agent_id"`
		Enabled         bool   `json:"enabled"`
	}
	if err := readJSON(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid JSON"})
		return
	}
	if strings.TrimSpace(req.EventKind) == "" {
		// Defaulted rather than refused: the operator's real question is "what
		// do I write in the recipe", and custom.<slug> is a good answer.
		req.EventKind = "custom." + strings.TrimSpace(req.Slug)
	}

	saved, err := s.store.SaveWebhookEndpoint(store.WebhookEndpoint{
		Slug: req.Slug, Name: req.Name, Description: req.Description,
		EventKind: req.EventKind, Secret: req.Secret, SignatureHeader: req.SignatureHeader,
		AgentID: req.AgentID, Enabled: req.Enabled, CreatedBy: consoleUser(r).Member,
	})
	if err != nil {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"error": err.Error()})
		return
	}

	s.audit(r, "human", consoleUser(r).Member, "console.webhook.create", saved.Slug, "", saved.EventKind)
	writeJSON(w, http.StatusOK, s.customRow(saved))
}

func (s *ConsoleServer) handleUpdateWebhook(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	existing, err := s.webhookByID(id)
	if err != nil || existing == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "no such webhook"})
		return
	}

	var req struct {
		Slug            *string `json:"slug"`
		Name            *string `json:"name"`
		Description     *string `json:"description"`
		EventKind       *string `json:"event_kind"`
		Secret          *string `json:"secret"`
		SignatureHeader *string `json:"signature_header"`
		AgentID         *string `json:"agent_id"`
		Enabled         *bool   `json:"enabled"`
	}
	if err := readJSON(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid JSON"})
		return
	}

	// Pointers throughout: an omitted field is left alone, and a field set to
	// "" genuinely clears it. Without the distinction, editing a name would
	// silently wipe the secret.
	e := *existing
	if req.Slug != nil {
		e.Slug = *req.Slug
	}
	if req.Name != nil {
		e.Name = *req.Name
	}
	if req.Description != nil {
		e.Description = *req.Description
	}
	if req.EventKind != nil {
		e.EventKind = *req.EventKind
	}
	if req.Secret != nil {
		e.Secret = *req.Secret
	}
	if req.SignatureHeader != nil {
		e.SignatureHeader = *req.SignatureHeader
	}
	if req.AgentID != nil {
		e.AgentID = *req.AgentID
	}
	if req.Enabled != nil {
		e.Enabled = *req.Enabled
	}

	saved, err := s.store.SaveWebhookEndpoint(e)
	if err != nil {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"error": err.Error()})
		return
	}
	s.audit(r, "human", consoleUser(r).Member, "console.webhook.update", saved.Slug, "", "")
	writeJSON(w, http.StatusOK, s.customRow(saved))
}

func (s *ConsoleServer) handleDeleteWebhook(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	existing, err := s.webhookByID(id)
	if err != nil || existing == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "no such webhook"})
		return
	}
	if err := s.store.DeleteWebhookEndpoint(id); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	s.audit(r, "human", consoleUser(r).Member, "console.webhook.delete", existing.Slug, "", "")
	writeJSON(w, http.StatusOK, map[string]any{"deleted": true})
}

// handleWebhookDeliveries shows what has actually arrived.
//
// The most common integration failure is a webhook that silently does nothing,
// and without this the only evidence is on the sender's side where the operator
// cannot see it.
func (s *ConsoleServer) handleWebhookDeliveries(w http.ResponseWriter, r *http.Request) {
	endpoint := strings.TrimSpace(r.URL.Query().Get("endpoint"))
	list, err := s.store.WebhookDeliveries(endpoint, queryInt(r, "limit", 50))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	out := make([]deliveryRow, 0, len(list))
	for _, d := range list {
		out = append(out, deliveryRow{
			ID: d.ID, Endpoint: d.Endpoint, Source: d.Source, Status: d.Status,
			Detail: d.Detail, BodySample: d.BodySample, ReceivedAt: rfc3339(d.ReceivedAt),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"deliveries": out})
}

func (s *ConsoleServer) webhookByID(id string) (*store.WebhookEndpoint, error) {
	all, err := s.store.ListWebhookEndpoints()
	if err != nil {
		return nil, err
	}
	for i := range all {
		if all[i].ID == id {
			return &all[i], nil
		}
	}
	return nil, nil
}

func (s *ConsoleServer) customRow(e store.WebhookEndpoint) webhookRow {
	return webhookRow{
		ID: e.ID, Kind: "custom", Name: e.Name, Slug: e.Slug,
		URL:       s.webhookBase() + webhook.CustomPrefix + e.Slug,
		EventKind: e.EventKind, Description: e.Description,
		Enabled: e.Enabled, Live: e.Enabled,
		// The secret itself is never returned. Whether one is set is what the
		// screen needs; the value is what an operator pasted once and should
		// not be able to read back out of a browser.
		Secured:         e.Secret != "",
		SignatureHeader: e.SignatureHeader,
		AgentID:         e.AgentID, CreatedBy: e.CreatedBy,
		UpdatedAt: rfc3339(e.UpdatedAt),
	}
}
