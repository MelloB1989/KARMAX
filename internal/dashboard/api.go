package dashboard

import (
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/MelloB1989/karmax/internal/agent"
	"github.com/MelloB1989/karmax/internal/bus"
	"github.com/MelloB1989/karmax/internal/memory"
	"github.com/MelloB1989/karmax/internal/scheduler"
	"github.com/MelloB1989/karmax/internal/store"
	"github.com/MelloB1989/karmax/internal/tools"
	"github.com/MelloB1989/karmax/internal/webhook"
	"github.com/go-chi/chi/v5"
)

type APIHandler struct {
	agents    *agent.Registry
	scheduler *scheduler.Scheduler
	webhooks  *webhook.WebhookServer
	memory    *memory.ManagerFactory
	tools     *tools.Registry
	store     *store.Store
	bus       *bus.Bus
}

func NewAPIHandler(
	agents *agent.Registry,
	sched *scheduler.Scheduler,
	wh *webhook.WebhookServer,
	mem *memory.ManagerFactory,
	toolReg *tools.Registry,
	s *store.Store,
	b *bus.Bus,
) *APIHandler {
	return &APIHandler{
		agents:    agents,
		scheduler: sched,
		webhooks:  wh,
		memory:    mem,
		tools:     toolReg,
		store:     s,
		bus:       b,
	}
}

func (h *APIHandler) RegisterRoutes(r chi.Router) {
	r.Get("/api/agents", h.listAgents)
	r.Get("/api/agents/{id}", h.getAgent)
	r.Post("/api/agents/{id}/start", h.startAgent)
	r.Post("/api/agents/{id}/stop", h.stopAgent)
	r.Post("/api/agents/{id}/pause", h.pauseAgent)
	r.Post("/api/agents/{id}/resume", h.resumeAgent)
	r.Post("/api/agents/{id}/restart", h.restartAgent)
	r.Post("/api/agents/{id}/trigger", h.triggerAgent)

	r.Get("/api/scheduler/jobs", h.listJobs)
	r.Post("/api/scheduler/jobs", h.createJob)
	r.Delete("/api/scheduler/jobs/{id}", h.deleteJob)
	r.Post("/api/scheduler/jobs/{id}/run", h.runJob)

	r.Get("/api/webhooks/routes", h.listWebhookRoutes)
	r.Get("/api/webhooks/events", h.listWebhookEvents)

	r.Get("/api/memory/{namespace}/search", h.searchMemory)
	r.Get("/api/memory/{namespace}/recent", h.recentMemory)

	r.Get("/api/tools", h.listTools)
	r.Post("/api/tools/{name}/execute", h.executeTool)

	r.Get("/api/events", h.listEvents)
}

func (h *APIHandler) listAgents(w http.ResponseWriter, r *http.Request) {
	snaps := h.agents.Snapshots()
	if snaps == nil {
		snaps = []agent.AgentSnapshot{}
	}
	writeJSON(w, snaps)
}

func (h *APIHandler) getAgent(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	a, ok := h.agents.Get(id)
	if !ok {
		http.Error(w, "agent not found", http.StatusNotFound)
		return
	}
	writeJSON(w, a.Snapshot())
}

func (h *APIHandler) startAgent(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	a, ok := h.agents.Get(id)
	if !ok {
		http.Error(w, "agent not found", http.StatusNotFound)
		return
	}
	if err := a.Start(r.Context()); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, map[string]string{"status": "started"})
}

func (h *APIHandler) stopAgent(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	a, ok := h.agents.Get(id)
	if !ok {
		http.Error(w, "agent not found", http.StatusNotFound)
		return
	}
	if err := a.Stop(); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, map[string]string{"status": "stopped"})
}

func (h *APIHandler) pauseAgent(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	a, ok := h.agents.Get(id)
	if !ok {
		http.Error(w, "agent not found", http.StatusNotFound)
		return
	}
	if err := a.Pause(); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, map[string]string{"status": "paused"})
}

func (h *APIHandler) resumeAgent(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	a, ok := h.agents.Get(id)
	if !ok {
		http.Error(w, "agent not found", http.StatusNotFound)
		return
	}
	if err := a.Resume(); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, map[string]string{"status": "resumed"})
}

func (h *APIHandler) restartAgent(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	a, ok := h.agents.Get(id)
	if !ok {
		http.Error(w, "agent not found", http.StatusNotFound)
		return
	}
	a.Stop()
	if err := a.Start(r.Context()); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, map[string]string{"status": "restarted"})
}

func (h *APIHandler) triggerAgent(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	a, ok := h.agents.Get(id)
	if !ok {
		http.Error(w, "agent not found", http.StatusNotFound)
		return
	}

	var payload map[string]any
	json.NewDecoder(r.Body).Decode(&payload)

	evt := bus.NewEvent(bus.EventUserDefined, id, payload)
	a.Send(evt)

	writeJSON(w, map[string]string{"status": "triggered", "event_id": evt.ID})
}

func (h *APIHandler) listJobs(w http.ResponseWriter, r *http.Request) {
	jobs := h.scheduler.ListJobs()
	if jobs == nil {
		writeJSON(w, []any{})
		return
	}
	writeJSON(w, jobs)
}

func (h *APIHandler) createJob(w http.ResponseWriter, r *http.Request) {
	var j scheduler.ScheduledJob
	if err := json.NewDecoder(r.Body).Decode(&j); err != nil {
		writeError(w, err)
		return
	}
	if err := h.scheduler.AddJob(j); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, map[string]string{"status": "created", "id": j.ID})
}

func (h *APIHandler) deleteJob(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if err := h.scheduler.RemoveJob(id); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, map[string]string{"status": "deleted"})
}

func (h *APIHandler) runJob(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if err := h.scheduler.RunJobNow(id); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, map[string]string{"status": "triggered"})
}

func (h *APIHandler) listWebhookRoutes(w http.ResponseWriter, r *http.Request) {
	routes := h.webhooks.ListRoutes()
	if routes == nil {
		writeJSON(w, []any{})
		return
	}
	writeJSON(w, routes)
}

func (h *APIHandler) listWebhookEvents(w http.ResponseWriter, r *http.Request) {
	limit := 50
	if l := r.URL.Query().Get("limit"); l != "" {
		if n, err := strconv.Atoi(l); err == nil {
			limit = n
		}
	}
	events, _ := h.store.ListWebhookEvents(limit)
	if events == nil {
		writeJSON(w, []any{})
		return
	}
	writeJSON(w, events)
}

func (h *APIHandler) searchMemory(w http.ResponseWriter, r *http.Request) {
	ns := chi.URLParam(r, "namespace")
	query := r.URL.Query().Get("query")
	topK := 5
	if k := r.URL.Query().Get("top_k"); k != "" {
		if n, err := strconv.Atoi(k); err == nil {
			topK = n
		}
	}

	mgr := h.memory.For("", ns)
	results, err := mgr.Search(query, topK)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, results)
}

func (h *APIHandler) recentMemory(w http.ResponseWriter, r *http.Request) {
	ns := chi.URLParam(r, "namespace")
	n := 50
	if nStr := r.URL.Query().Get("n"); nStr != "" {
		if parsed, err := strconv.Atoi(nStr); err == nil {
			n = parsed
		}
	}

	mgr := h.memory.For("", ns)
	entries, err := mgr.Recent(n)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, entries)
}

func (h *APIHandler) listTools(w http.ResponseWriter, r *http.Request) {
	toolList := h.tools.List()
	if toolList == nil {
		writeJSON(w, []any{})
		return
	}
	writeJSON(w, toolList)
}

func (h *APIHandler) executeTool(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	t, ok := h.tools.Get(name)
	if !ok {
		http.Error(w, "tool not found", http.StatusNotFound)
		return
	}

	var input map[string]any
	json.NewDecoder(r.Body).Decode(&input)

	result, err := t.Execute(r.Context(), input)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, result)
}

func (h *APIHandler) listEvents(w http.ResponseWriter, r *http.Request) {
	limit := 50
	if l := r.URL.Query().Get("limit"); l != "" {
		if n, err := strconv.Atoi(l); err == nil {
			limit = n
		}
	}
	kind := r.URL.Query().Get("kind")
	events, _ := h.store.ListEvents(limit, kind)
	if events == nil {
		writeJSON(w, []any{})
		return
	}
	writeJSON(w, events)
}

func writeJSON(w http.ResponseWriter, data any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(data)
}

func writeError(w http.ResponseWriter, err error) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusInternalServerError)
	json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
}
