package api

import (
	"net/http"
	"sort"
	"strings"

	"github.com/MelloB1989/karmax/internal/store"
	"github.com/MelloB1989/karmax/internal/webhook"
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

// handleListWebhooks returns the endpoints an operator has created.
//
// Connector-declared paths are deliberately NOT listed. They are mounted by
// their connector and cannot be edited or deleted from here, so showing them
// in a list with Enable and Delete buttons promised something the page could
// not do. Every row here is one somebody made and can change.
func (s *ConsoleServer) handleListWebhooks(w http.ResponseWriter, r *http.Request) {
	custom, err := s.store.ListWebhookEndpoints()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}

	out := make([]webhookRow, 0, len(custom))
	for _, e := range custom {
		out = append(out, s.customRow(e))
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	writeJSON(w, http.StatusOK, map[string]any{"webhooks": out, "base_url": s.webhookBase()})
}

// handleWebhookCatalogue is what the create form builds its dropdowns from.
//
// Typing a signature header or an event kind by hand produces a webhook that
// silently never fires, with nothing on screen to say which of the two was
// wrong. Everything KARMAX supports is offered instead.
func (s *ConsoleServer) handleWebhookCatalogue(w http.ResponseWriter, r *http.Request) {
	agents := []string{}
	if s.agents != nil {
		for _, a := range s.agents.List() {
			agents = append(agents, a.Def().ID)
		}
	}
	sort.Strings(agents)

	writeJSON(w, http.StatusOK, map[string]any{
		"platforms":         webhook.Platforms(),
		"signature_headers": webhook.SignatureHeaders(),
		"agents":            agents,
		"base_url":          s.webhookBase(),
	})
}

func (s *ConsoleServer) handleCreateWebhook(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Slug            string `json:"slug"`
		Name            string `json:"name"`
		Description     string `json:"description"`
		Platform        string `json:"platform"`
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
	plat, known := webhook.PlatformByID(strings.TrimSpace(req.Platform))
	if !known {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
			"error": "unknown platform " + req.Platform + " — pick one KARMAX supports, or Custom",
		})
		return
	}
	if plat.ID != "" {
		// A platform decides its own event kind and how a delivery proves
		// itself. Letting those be typed is how a webhook ends up silently
		// never firing.
		req.EventKind = plat.EventKind
		req.SignatureHeader = plat.SignatureHeader
	}
	if strings.TrimSpace(req.EventKind) == "" {
		// Defaulted rather than refused: the operator's real question is "what
		// do I write in the recipe", and custom.<slug> is a good answer.
		req.EventKind = "custom." + strings.TrimSpace(req.Slug)
	}

	saved, err := s.store.SaveWebhookEndpoint(store.WebhookEndpoint{
		Slug: req.Slug, Name: req.Name, Description: req.Description, Platform: plat.ID,
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
	kind, connector := "custom", ""
	if e.Platform != "" {
		kind, connector = "platform", e.Platform
	}
	return webhookRow{
		ID: e.ID, Kind: kind, Connector: connector, Name: e.Name, Slug: e.Slug,
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
