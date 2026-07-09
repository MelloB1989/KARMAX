package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/MelloB1989/karmax/internal/agent"
	"github.com/MelloB1989/karmax/internal/hostpaths"
	"github.com/MelloB1989/karmax/internal/config"
	"github.com/MelloB1989/karmax/internal/memory"
	"github.com/MelloB1989/karmax/internal/scheduler"
	"github.com/MelloB1989/karmax/internal/store"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

// Version is reported by /api/ping so clients can sanity-check the server.
const Version = "0.2.0"

// Server is the HTTP API the KARMAX phone app talks to. It binds to 0.0.0.0 so
// it is reachable over both the LAN and Tailscale.
type Server struct {
	addr      string
	port      int
	token     string
	agents    *agent.Registry
	store     *store.Store
	scheduler *scheduler.Scheduler
	mem       *memory.ManagerFactory
	cfg       *config.KarmaxConfig
	log       *zap.Logger
	httpSrv   *http.Server
	mdns      *mdnsAd
	runLoop   func(name string) (bool, error) // injected: run a loopkit loop by name
	listLoops func() []LoopInfo               // injected: the daemon's ACTIVE loops
}

// LoopInfo describes one active loop for GET /api/loops.
type LoopInfo struct {
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Schedule    string   `json:"schedule"`
	Webhook     string   `json:"webhook,omitempty"`
	Events      []string `json:"events,omitempty"`
}

// SetRunLoop wires the manual loop-run callback (POST /api/loops/{name}/run).
func (s *Server) SetRunLoop(fn func(name string) (bool, error)) { s.runLoop = fn }

// SetListLoops wires the live loop listing (GET /api/loops). This is the
// daemon's truth — it includes runtime-registered loops (e.g. cold-scan) and
// excludes operator-disabled ones, unlike a CLI process's local registry.
func (s *Server) SetListLoops(fn func() []LoopInfo) { s.listLoops = fn }

// New builds the API server. token (from KARMAX_API_TOKEN) gates everything
// except /api/ping; an empty token disables auth (development only).
func New(addr string, port int, token string, agents *agent.Registry, s *store.Store, sched *scheduler.Scheduler, mem *memory.ManagerFactory, cfg *config.KarmaxConfig, log *zap.Logger) *Server {
	srv := &Server{addr: addr, port: port, token: strings.TrimSpace(token), agents: agents, store: s, scheduler: sched, mem: mem, cfg: cfg, log: log}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/ping", srv.handlePing)
	mux.HandleFunc("/api/chat", srv.auth(srv.handleChat))
	mux.HandleFunc("/api/messages", srv.auth(srv.handleMessages))
	mux.HandleFunc("POST /api/conversation/reset", srv.auth(srv.handleResetConversation))
	mux.HandleFunc("/api/push/register", srv.auth(srv.handlePushRegister))
	mux.HandleFunc("GET /api/proposals", srv.auth(srv.handleProposals))
	mux.HandleFunc("POST /api/proposals/{id}/decision", srv.auth(srv.handleProposalDecision))
	mux.HandleFunc("GET /api/notifications", srv.auth(srv.handleNotifications))
	mux.HandleFunc("POST /api/notifications/read-all", srv.auth(srv.handleReadAllNotifications))
	mux.HandleFunc("GET /api/reviews", srv.auth(srv.handleListReviews))
	mux.HandleFunc("POST /api/reviews/{id}/answer", srv.auth(srv.handleAnswerReview))
	mux.HandleFunc("POST /api/notifications/{id}/read", srv.auth(srv.handleReadNotification))
	mux.HandleFunc("GET /api/activity", srv.auth(srv.handleActivity))
	mux.HandleFunc("POST /api/jobs/{id}/run", srv.auth(srv.handleRunJob))
	mux.HandleFunc("POST /api/loops/{name}/run", srv.auth(srv.handleRunLoop))
	mux.HandleFunc("GET /api/loops", srv.auth(srv.handleListLoops))
	mux.HandleFunc("GET /api/memory/tree", srv.auth(srv.handleMemoryTree))
	mux.HandleFunc("GET /api/memory/entries", srv.auth(srv.handleMemoryEntries))
	mux.HandleFunc("GET /api/memory/cleanup/question", srv.auth(srv.handleCleanupQuestion))
	mux.HandleFunc("POST /api/memory/cleanup/answer", srv.auth(srv.handleCleanupAnswer))
	mux.HandleFunc("GET /api/memory/graph", srv.auth(srv.handleMemoryGraph))
	mux.HandleFunc("POST /api/memory/graph/rebuild", srv.auth(srv.handleRebuildGraph))
	mux.HandleFunc("POST /api/contacts/sync", srv.auth(srv.handleSyncContacts))
	mux.HandleFunc("GET /api/contacts", srv.auth(srv.handleGetContacts))
	mux.HandleFunc("DELETE /api/memory/entries/{id}", srv.auth(srv.handleDeleteMemoryEntry))
	mux.HandleFunc("GET /api/profile", srv.auth(srv.handleGetProfile))
	mux.HandleFunc("PUT /api/profile", srv.auth(srv.handlePutProfile))
	mux.HandleFunc("GET /api/device/actions", srv.auth(srv.handleDeviceActions))
	mux.HandleFunc("POST /api/device/actions/{id}/complete", srv.auth(srv.handleCompleteDeviceAction))
	mux.HandleFunc("GET /api/integrations", srv.auth(srv.handleIntegrations))
	mux.HandleFunc("GET /api/tools", srv.auth(srv.handleListTools))
	mux.HandleFunc("POST /api/tools/{name}", srv.auth(srv.handleCallTool))

	srv.httpSrv = &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	return srv
}

func (s *Server) Start(ctx context.Context) error {
	s.mdns = advertiseMDNS(s.port, s.log)
	s.startGraphMaintainer(ctx)

	go func() {
		<-ctx.Done()
		s.Stop()
	}()

	s.log.Info("api server listening", zap.String("addr", s.addr), zap.Bool("auth", s.token != ""))
	if err := s.httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

func (s *Server) Stop() {
	s.mdns.stop()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = s.httpSrv.Shutdown(ctx)
}

// auth wraps a handler with bearer-token authentication when a token is set.
func (s *Server) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.token != "" {
			header := r.Header.Get("Authorization")
			supplied := strings.TrimSpace(strings.TrimPrefix(header, "Bearer "))
			if !strings.HasPrefix(header, "Bearer ") || supplied != s.token {
				writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "unauthorized"})
				return
			}
		}
		next(w, r)
	}
}

func (s *Server) handlePing(w http.ResponseWriter, r *http.Request) {
	agentID := ""
	if a := s.defaultAgent(); a != nil {
		agentID = a.Def().ID
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"service":   "karmax",
		"version":   Version,
		"agent":     agentID,
		"auth":      s.token != "",
		"addresses": localAddresses(s.port),
		"time":      time.Now().UTC().Format(time.RFC3339),
	})
}

func (s *Server) handleChat(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "POST required"})
		return
	}
	var req struct {
		Message string `json:"message"`
		Agent   string `json:"agent"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid json"})
		return
	}
	if strings.TrimSpace(req.Message) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "message is required"})
		return
	}
	ag := s.resolveAgent(req.Agent)
	if ag == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "no agent available"})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Minute)
	defer cancel()

	reply, err := ag.Chat(ctx, req.Message)
	if err != nil {
		s.log.Error("api chat failed", zap.String("agent", ag.Def().ID), zap.Error(err))
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}

	// Record the app conversation turn (separate from the agent's working history).
	aid := ag.Def().ID
	_ = s.store.AppendAppMessage(store.StoredAppMessage{ID: uuid.New().String(), AgentID: aid, Role: "user", Content: req.Message})
	_ = s.store.AppendAppMessage(store.StoredAppMessage{ID: uuid.New().String(), AgentID: aid, Role: "assistant", Content: reply})

	writeJSON(w, http.StatusOK, map[string]any{"reply": reply, "agent": aid})
}

// handleResetConversation clears the phone-app conversation thread (the agent's
// memory and working history are untouched — KARMAX still remembers everything).
func (s *Server) handleResetConversation(w http.ResponseWriter, r *http.Request) {
	ag := s.resolveAgent(r.URL.Query().Get("agent"))
	if ag == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "no agent available"})
		return
	}
	if err := s.store.ClearAppMessages(ag.Def().ID); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"reset": true, "agent": ag.Def().ID})
}

func (s *Server) handleMessages(w http.ResponseWriter, r *http.Request) {
	ag := s.resolveAgent(r.URL.Query().Get("agent"))
	if ag == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "no agent available"})
		return
	}
	limit := 50
	if l := r.URL.Query().Get("limit"); l != "" {
		if n, err := strconv.Atoi(l); err == nil && n > 0 {
			limit = n
		}
	}
	msgs, err := s.store.LoadAppMessages(ag.Def().ID, limit)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	out := make([]map[string]any, 0, len(msgs))
	for _, m := range msgs {
		if m.Role == "tool" {
			continue
		}
		out = append(out, map[string]any{
			"role":       m.Role,
			"content":    m.Content,
			"created_at": m.CreatedAt.Format(time.RFC3339),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"agent": ag.Def().ID, "messages": out})
}

func (s *Server) handlePushRegister(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "POST required"})
		return
	}
	var req struct {
		Token    string `json:"token"`
		Platform string `json:"platform"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.Token) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "token is required"})
		return
	}
	if err := s.store.RegisterPushToken(req.Token, req.Platform); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"registered": true})
}

func (s *Server) handleProposals(w http.ResponseWriter, r *http.Request) {
	status := r.URL.Query().Get("status")
	limit := 50
	if l := r.URL.Query().Get("limit"); l != "" {
		if n, err := strconv.Atoi(l); err == nil && n > 0 {
			limit = n
		}
	}
	ps, err := s.store.ListProposals(status, limit)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	out := make([]map[string]any, 0, len(ps))
	for _, p := range ps {
		out = append(out, proposalJSON(p))
	}
	writeJSON(w, http.StatusOK, map[string]any{"proposals": out})
}

func (s *Server) handleProposalDecision(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req struct {
		Decision string `json:"decision"`
		Edit     string `json:"edit"`
		Note     string `json:"note"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid json"})
		return
	}

	p, err := s.store.GetProposal(id)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	if p == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "proposal not found"})
		return
	}
	if p.Status != "pending" {
		writeJSON(w, http.StatusConflict, map[string]any{"error": "already " + p.Status, "proposal": proposalJSON(*p)})
		return
	}

	if req.Decision == "reject" {
		_ = s.store.DecideProposal(id, "rejected", req.Note)
		// Reject WITH feedback = revise, not drop: feed the feedback back to the
		// agent so it reworks its approach and (if still sensible) submits a NEW
		// proposal via the propose tool. Plain reject (no note) just drops it.
		if strings.TrimSpace(req.Note) != "" {
			if ag := s.resolveAgent(""); ag != nil {
				prompt := fmt.Sprintf(
					"The operator REJECTED your proposed action and left feedback. Rework your approach using that feedback. If a revised action still makes sense, call the `propose` tool to submit a NEW proposal that incorporates the feedback (do not repeat the rejected version). If the feedback means they don't want this at all, just acknowledge and do not re-propose.\n\nRejected title: %s\nRejected action: %s\nOperator feedback: %s",
					p.Title, p.ProposedAction, req.Note,
				)
				go func() {
					ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
					defer cancel()
					if _, gerr := ag.Chat(ctx, prompt); gerr != nil {
						s.log.Warn("proposal revision failed", zap.String("proposal", id), zap.Error(gerr))
					}
				}()
			}
		}
		updated, _ := s.store.GetProposal(id)
		writeJSON(w, http.StatusOK, map[string]any{"proposal": proposalJSON(*updated)})
		return
	}
	if req.Decision != "approve" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "decision must be 'approve' or 'reject'"})
		return
	}

	action := p.ProposedAction
	if strings.TrimSpace(req.Edit) != "" {
		action = req.Edit
	}
	_ = s.store.DecideProposal(id, "approved", req.Note)

	ag := s.resolveAgent("")
	if ag == nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "no agent available"})
		return
	}

	prompt := fmt.Sprintf(
		"The operator APPROVED this proposed action. Execute it now using your tools, then confirm exactly what you did.\n\nTitle: %s\nAction: %s",
		p.Title, action,
	)
	if strings.TrimSpace(req.Note) != "" {
		prompt += "\nNote from the operator: " + req.Note
	}

	// Execute in the background so the decision request returns immediately;
	// the app polls and sees the status flip to executed/failed.
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		reply, gerr := ag.Chat(ctx, prompt)
		if gerr != nil {
			_ = s.store.SetProposalResult(id, "failed", gerr.Error())
			return
		}
		_ = s.store.SetProposalResult(id, "executed", reply)
	}()

	updated, _ := s.store.GetProposal(id)
	writeJSON(w, http.StatusOK, map[string]any{"proposal": proposalJSON(*updated)})
}

func proposalJSON(p store.StoredProposal) map[string]any {
	m := map[string]any{
		"id":         p.ID,
		"kind":       p.Kind,
		"title":      p.Title,
		"summary":    p.Summary,
		"context":    p.Context,
		"action":     p.ProposedAction,
		"status":     p.Status,
		"note":       p.DecisionNote,
		"result":     p.Result,
		"created_at": p.CreatedAt.Format(time.RFC3339),
	}
	if p.DecidedAt != nil {
		m["decided_at"] = p.DecidedAt.Format(time.RFC3339)
	}
	return m
}

func (s *Server) handleNotifications(w http.ResponseWriter, r *http.Request) {
	limit := 50
	if l := r.URL.Query().Get("limit"); l != "" {
		if n, err := strconv.Atoi(l); err == nil && n > 0 {
			limit = n
		}
	}
	ns, err := s.store.ListNotifications(limit)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	unread, _ := s.store.CountUnreadNotifications()
	out := make([]map[string]any, 0, len(ns))
	for _, n := range ns {
		out = append(out, notificationJSON(n))
	}
	writeJSON(w, http.StatusOK, map[string]any{"notifications": out, "unread": unread})
}

func (s *Server) handleReadNotification(w http.ResponseWriter, r *http.Request) {
	if err := s.store.MarkNotificationRead(r.PathValue("id")); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	unread, _ := s.store.CountUnreadNotifications()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "unread": unread})
}

func (s *Server) handleReadAllNotifications(w http.ResponseWriter, r *http.Request) {
	if err := s.store.MarkAllNotificationsRead(); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "unread": 0})
}

func notificationJSON(n store.StoredNotification) map[string]any {
	m := map[string]any{
		"id":         n.ID,
		"kind":       n.Kind,
		"title":      n.Title,
		"body":       n.Body,
		"data":       n.Data,
		"read":       n.ReadAt != nil,
		"created_at": n.CreatedAt.Format(time.RFC3339),
	}
	if n.ReadAt != nil {
		m["read_at"] = n.ReadAt.Format(time.RFC3339)
	}
	return m
}

func (s *Server) handleActivity(w http.ResponseWriter, r *http.Request) {
	jobs := s.scheduler.ListJobs()
	jobsOut := make([]map[string]any, 0, len(jobs))
	for _, j := range jobs {
		m := map[string]any{"id": j.ID, "name": j.Name, "cron": j.Cron, "agent": j.AgentID, "enabled": j.Enabled, "run_count": j.RunCount}
		if j.NextRun != nil {
			m["next_run"] = j.NextRun.Format(time.RFC3339)
		}
		if j.LastRun != nil {
			m["last_run"] = j.LastRun.Format(time.RFC3339)
		}
		jobsOut = append(jobsOut, m)
	}

	webhooks := make([]map[string]any, 0)
	if whs, err := s.store.ListWebhookEvents(20); err == nil {
		for _, wh := range whs {
			webhooks = append(webhooks, map[string]any{"id": wh.ID, "route": wh.Route, "method": wh.Method, "received_at": wh.ReceivedAt.Format(time.RFC3339)})
		}
	}

	sessions := make([]map[string]any, 0)
	if ag := s.defaultAgent(); ag != nil {
		if cs, err := s.store.ListCodingSessions(ag.Def().ID); err == nil {
			for _, c := range cs {
				sessions = append(sessions, map[string]any{"id": c.ID, "tool": c.ToolType, "description": c.Description, "status": c.Status, "session_id": c.SessionID, "updated_at": c.UpdatedAt.Format(time.RFC3339)})
			}
		}
	}

	events := make([]map[string]any, 0)
	if evs, err := s.store.ListEvents(30, ""); err == nil {
		for _, e := range evs {
			events = append(events, map[string]any{"id": e.ID, "kind": e.Kind, "agent": e.AgentID, "created_at": e.CreatedAt.Format(time.RFC3339)})
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"jobs":            jobsOut,
		"webhooks":        webhooks,
		"coding_sessions": sessions,
		"events":          events,
	})
}

func (s *Server) handleRunJob(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.scheduler.RunJobNow(id); err != nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ran": id})
}

// handleListLoops returns the loops actually running in this daemon.
func (s *Server) handleListLoops(w http.ResponseWriter, r *http.Request) {
	if s.listLoops == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "loops not available"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"loops": s.listLoops()})
}

// handleRunLoop runs a loopkit loop by name on demand (manual trigger).
func (s *Server) handleRunLoop(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if s.runLoop == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "loops not available"})
		return
	}
	ran, err := s.runLoop(name)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	if !ran {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "no such loop: " + name})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ran": name})
}

func (s *Server) defaultNamespace() string {
	ag := s.defaultAgent()
	if ag == nil {
		return ""
	}
	ns := ag.Def().Memory.Namespace
	if ns == "" {
		ns = ag.Def().ID
	}
	return ns
}

// handleListReviews returns the open staleness check-ins for the app's review
// inbox. Answering one (here or via WhatsApp) closes it everywhere.
func (s *Server) handleListReviews(w http.ResponseWriter, r *http.Request) {
	ns := s.defaultNamespace()
	reviews, err := s.store.ListOpenReviews(ns, 50)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	out := make([]map[string]any, 0, len(reviews))
	for _, rv := range reviews {
		var opts []string
		_ = json.Unmarshal([]byte(rv.Options), &opts)
		out = append(out, map[string]any{
			"id": rv.ID, "kind": rv.TargetKind, "question": rv.Question,
			"options": opts, "created_at": rv.CreatedAt.Format(time.RFC3339),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"reviews": out})
}

// handleAnswerReview resolves a review from the app: records the answer, closes
// it, and applies the consequence to the underlying memory (keep/update/forget).
func (s *Server) handleAnswerReview(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req struct {
		Answer     string `json:"answer"`
		Resolution string `json:"resolution"` // kept | updated | forgotten | done | dropped
		NewContent string `json:"new_content"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)

	rv, err := s.store.GetReview(id)
	if err != nil || rv == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "review not found"})
		return
	}
	if rv.Status != "open" {
		writeJSON(w, http.StatusOK, map[string]any{"status": "already_resolved"})
		return
	}
	status := "resolved"
	if req.Resolution == "dropped" || req.Resolution == "forgotten" {
		status = "dismissed"
	}
	if err := s.store.ResolveReview(id, status, req.Answer, req.Resolution); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	// Apply to memory (reminders just close).
	if rv.TargetKind == "memory" && rv.TargetID != "" {
		switch req.Resolution {
		case "forgotten", "done", "dropped":
			_ = s.store.DeleteMemoryEntry(rv.TargetID)
		case "updated":
			if strings.TrimSpace(req.NewContent) != "" {
				_ = s.store.DeleteMemoryEntry(rv.TargetID)
				if mgr := s.memManager(); mgr != nil {
					_ = mgr.Write(memory.MemoryEntry{Role: "assistant", Content: strings.TrimSpace(req.NewContent), Tags: []string{"reviewed"}})
				}
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": status, "resolution": req.Resolution})
}

func (s *Server) memManager() *memory.Manager {
	ag := s.defaultAgent()
	if ag == nil || s.mem == nil {
		return nil
	}
	return s.mem.For(ag.Def().ID, s.defaultNamespace())
}

func (s *Server) handleMemoryTree(w http.ResponseWriter, r *http.Request) {
	ns := s.defaultNamespace()
	// A missing tree (fresh namespace) is not an error — return an empty tree.
	treeJSON, _, _ := s.store.LoadPageIndexTree(ns)
	var tree any
	if treeJSON != "" {
		_ = json.Unmarshal([]byte(treeJSON), &tree)
	}
	writeJSON(w, http.StatusOK, map[string]any{"namespace": ns, "tree": tree})
}

func (s *Server) handleMemoryEntries(w http.ResponseWriter, r *http.Request) {
	ns := s.defaultNamespace()
	limit := 100
	if l := r.URL.Query().Get("limit"); l != "" {
		if n, err := strconv.Atoi(l); err == nil && n > 0 {
			limit = n
		}
	}
	q := strings.TrimSpace(r.URL.Query().Get("q"))

	var (
		entries []store.StoredMemoryEntry
		err     error
	)
	if q != "" {
		entries, err = s.store.SearchMemoryEntries(ns, q, limit)
	} else {
		entries, err = s.store.ListMemoryEntries(ns, limit)
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}

	out := make([]map[string]any, 0, len(entries))
	for _, e := range entries {
		var tags []string
		_ = json.Unmarshal([]byte(e.Tags), &tags)
		out = append(out, map[string]any{
			"id":         e.ID,
			"role":       e.Role,
			"content":    e.Content,
			"tags":       tags,
			"created_at": e.CreatedAt.Format(time.RFC3339),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"namespace": ns, "entries": out})
}

func (s *Server) handleDeleteMemoryEntry(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.store.DeleteMemoryEntry(id); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": id})
}

func (s *Server) handleGetProfile(w http.ResponseWriter, r *http.Request) {
	mgr := s.memManager()
	if mgr == nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "memory not available"})
		return
	}
	content, err := mgr.ReadProfile()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"profile": content})
}

func (s *Server) handlePutProfile(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Content string `json:"content"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid json"})
		return
	}
	mgr := s.memManager()
	if mgr == nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "memory not available"})
		return
	}
	if err := mgr.WriteProfile(req.Content); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"saved": true})
}

func (s *Server) handleDeviceActions(w http.ResponseWriter, r *http.Request) {
	status := r.URL.Query().Get("status")
	limit := 50
	if l := r.URL.Query().Get("limit"); l != "" {
		if n, err := strconv.Atoi(l); err == nil && n > 0 {
			limit = n
		}
	}
	actions, err := s.store.ListDeviceActions(status, limit)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	out := make([]map[string]any, 0, len(actions))
	for _, a := range actions {
		var payload any
		_ = json.Unmarshal([]byte(a.Payload), &payload)
		out = append(out, map[string]any{
			"id":         a.ID,
			"kind":       a.Kind,
			"payload":    payload,
			"status":     a.Status,
			"result":     a.Result,
			"created_at": a.CreatedAt.Format(time.RFC3339),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"actions": out})
}

func (s *Server) handleCompleteDeviceAction(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req struct {
		Status string `json:"status"`
		Result string `json:"result"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	status := req.Status
	if status == "" {
		status = "done"
	}
	if err := s.store.CompleteDeviceAction(id, status, req.Result); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "status": status})
}

func (s *Server) handleIntegrations(w http.ResponseWriter, r *http.Request) {
	out := make([]map[string]any, 0, 8)
	add := func(id, name, status, detail string) {
		out = append(out, map[string]any{"id": id, "name": name, "status": status, "detail": detail})
	}

	// WhatsApp (wacli) — live check.
	waPath, waConfigured := "", false
	if s.cfg != nil {
		for _, ch := range s.cfg.Comms.Channels {
			if ch.Type == "whatsapp" {
				waConfigured = true
				if p := ch.Settings["wacli_path"]; p != "" {
					waPath = p
				}
			}
		}
	}
	if waConfigured {
		if waPath == "" {
			waPath = hostpaths.Wacli()
		}
		status, detail := "disconnected", "wacli not responding"
		ctx, cancel := context.WithTimeout(r.Context(), 6*time.Second)
		raw, err := exec.CommandContext(ctx, waPath, "status").CombinedOutput()
		cancel()
		txt := strings.ToLower(strings.TrimSpace(string(raw)))
		switch {
		case err == nil && (strings.Contains(txt, "connected") || strings.Contains(txt, "online") || strings.Contains(txt, "logged in") || strings.Contains(txt, "ready")):
			status, detail = "connected", "wacli online"
		case err == nil:
			detail = firstLine(string(raw))
		}
		add("whatsapp", "WhatsApp", status, detail)
	} else {
		add("whatsapp", "WhatsApp", "off", "not configured")
	}

	// Discord
	discord := false
	if s.cfg != nil {
		for _, ch := range s.cfg.Comms.Channels {
			if ch.Type == "discord" && strings.TrimSpace(ch.Token) != "" {
				discord = true
			}
		}
	}
	if discord {
		add("discord", "Discord", "configured", "bot token set")
	} else {
		add("discord", "Discord", "off", "not configured")
	}

	// Google Workspace
	if gws := lookGWS(); gws != "" {
		add("google_workspace", "Google Workspace", "available", gws)
	} else {
		add("google_workspace", "Google Workspace", "missing", "install + auth the gws CLI")
	}

	// Coding harnesses
	if p, err := exec.LookPath("claude"); err == nil {
		add("claude_code", "Claude Code", "available", p)
	} else {
		add("claude_code", "Claude Code", "missing", "claude CLI not on PATH")
	}
	if p, err := exec.LookPath("codex"); err == nil {
		add("codex", "Codex", "available", p)
	} else {
		add("codex", "Codex", "missing", "codex CLI not on PATH")
	}

	// AI gateway
	base := ""
	if s.cfg != nil {
		base = s.cfg.AI.Providers["anthropic"].BaseURL
	}
	if base == "" {
		base = os.Getenv("ANTHROPIC_BASE_URL")
	}
	if base != "" {
		status := "offline"
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		if req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(base, "/")+"/v1/models", nil); err == nil {
			if resp, derr := http.DefaultClient.Do(req); derr == nil {
				resp.Body.Close()
				status = "online"
			}
		}
		cancel()
		add("gateway", "AI gateway", status, base)
	} else {
		add("gateway", "AI gateway", "unknown", "no base url")
	}

	// Phone push
	if toks, err := s.store.ListPushTokens(); err == nil && len(toks) > 0 {
		add("push", "Phone push", "registered", fmt.Sprintf("%d device(s)", len(toks)))
	} else {
		add("push", "Phone push", "none", "open the app to register")
	}

	// ntfy
	if topic := os.Getenv("NTFY_TOPIC"); topic != "" {
		add("ntfy", "ntfy", "configured", topic)
	} else {
		add("ntfy", "ntfy", "off", "NTFY_TOPIC not set")
	}

	writeJSON(w, http.StatusOK, map[string]any{"integrations": out})
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 120 {
		s = s[:120]
	}
	if s == "" {
		return "no output"
	}
	return s
}

func lookGWS() string {
	p := hostpaths.GWS()
	// hostpaths falls back to the bare command name; only report it as
	// available if it actually resolves to something runnable.
	if p == "gws" {
		if _, err := exec.LookPath(p); err != nil {
			return ""
		}
	}
	if _, err := os.Stat(p); err != nil {
		if _, lerr := exec.LookPath(p); lerr != nil {
			return ""
		}
	}
	return p
}

// handleListTools returns the manifest of every tool the (default or named)
// agent's model runs with — the full harness toolset, including agent-scoped
// memory/profile tools and MCP tools.
func (s *Server) handleListTools(w http.ResponseWriter, r *http.Request) {
	ag := s.resolveAgent(r.URL.Query().Get("agent"))
	if ag == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "no agent available"})
		return
	}
	manifests := ag.ToolManifests()
	out := make([]map[string]any, 0, len(manifests))
	for _, m := range manifests {
		var params any
		_ = json.Unmarshal(m.Parameters, &params)
		out = append(out, map[string]any{
			"name":        m.Name,
			"description": m.Description,
			"parameters":  params,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"agent": ag.Def().ID, "tools": out})
}

// handleCallTool executes one of the agent's tools by name with a JSON input
// body — the exact call the harness model would make. This is what gives the
// karmax CLI (and delegated coding harnesses) full parity with the harness.
func (s *Server) handleCallTool(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	ag := s.resolveAgent(r.URL.Query().Get("agent"))
	if ag == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "no agent available"})
		return
	}

	input := map[string]any{}
	if r.Body != nil {
		body, err := io.ReadAll(io.LimitReader(r.Body, 4<<20))
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "read body: " + err.Error()})
			return
		}
		if len(strings.TrimSpace(string(body))) > 0 {
			if err := json.Unmarshal(body, &input); err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]any{"error": "input must be a JSON object: " + err.Error()})
				return
			}
		}
	}

	// Tool runs can be slow (coding harness delegation) — allow a generous window.
	ctx, cancel := context.WithTimeout(r.Context(), 12*time.Minute)
	defer cancel()

	res, err := ag.ExecuteTool(ctx, name, input)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": err.Error()})
		return
	}
	if res.IsError {
		writeJSON(w, http.StatusOK, map[string]any{"tool": name, "ok": false, "error": res.Error})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"tool": name, "ok": true, "output": res.Output})
}

func (s *Server) defaultAgent() *agent.Agent {
	agents := s.agents.List()
	if len(agents) == 0 {
		return nil
	}
	return agents[0]
}

func (s *Server) resolveAgent(id string) *agent.Agent {
	if strings.TrimSpace(id) != "" {
		if a, ok := s.agents.Get(id); ok {
			return a
		}
		return nil
	}
	return s.defaultAgent()
}

// localAddresses returns the http base URLs a client can use to reach KARMAX
// after first contact: the stable mDNS name plus the host's real LAN IPv4s
// (physical WiFi/Ethernet — docker/bridge/VPN interfaces are excluded, since a
// phone on the same WiFi can't reach those and would just waste probes on them).
func localAddresses(port int) []string {
	out := []string{fmt.Sprintf("http://%s.local:%d", mdnsHost(), port)}
	for _, ip := range localIPv4s() {
		out = append(out, fmt.Sprintf("http://%s:%d", ip, port))
	}
	return out
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
