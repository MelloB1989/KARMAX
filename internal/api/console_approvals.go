package api

import (
	"net/http"
	"strings"

	"github.com/MelloB1989/karmax/internal/store"
)

// Approvals are the same proposals the phone app and Slack show — deliberately
// not a second mechanism. What the console adds is a name: a shared API token
// cannot say which human approved, and the whole point of an audit trail is
// that it can.

type consoleApproval struct {
	ID        string  `json:"id"`
	Kind      string  `json:"kind"`
	Title     string  `json:"title"`
	Summary   string  `json:"summary"`
	Context   string  `json:"context"`
	Action    string  `json:"action"`
	CaseID    *string `json:"case_id"`
	CaseKey   *string `json:"case_key"`
	Agent     string  `json:"agent"`
	Role      string  `json:"role"`
	Channel   string  `json:"channel"`
	Status    string  `json:"status"`
	Note      string  `json:"note"`
	Result    string  `json:"result"`
	CreatedAt string  `json:"created_at"`
	DecidedAt *string `json:"decided_at"`
	DecidedBy *string `json:"decided_by"`
}

func (s *ConsoleServer) toApproval(p store.StoredProposal) consoleApproval {
	a := consoleApproval{
		ID: p.ID, Kind: p.Kind, Title: p.Title, Summary: p.Summary,
		Context: p.Context, Action: p.ProposedAction, Agent: p.AgentID,
		Status: p.Status, Note: p.DecisionNote, Result: p.Result,
		CreatedAt: rfc3339(p.CreatedAt),
	}
	if p.DecidedAt != nil {
		a.DecidedAt = rfc3339Ptr(*p.DecidedAt)
	}
	if p.DecidedBy != "" {
		by := p.DecidedBy
		a.DecidedBy = &by
	}

	// Case linkage is genuinely absent from the proposal row rather than
	// unknown, so these stay null instead of being guessed at from the title.
	return a
}

func (s *ConsoleServer) handleConsoleApprovals(w http.ResponseWriter, r *http.Request) {
	proposals, err := s.store.ListProposals(r.URL.Query().Get("status"), queryInt(r, "limit", 100))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	out := make([]consoleApproval, 0, len(proposals))
	for _, p := range proposals {
		out = append(out, s.toApproval(p))
	}
	writeJSON(w, http.StatusOK, map[string]any{"approvals": out})
}

func (s *ConsoleServer) handleConsoleApprovalDecision(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req struct {
		Decision string `json:"decision"`
		Note     string `json:"note"`
	}
	if err := readJSON(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid JSON"})
		return
	}
	if req.Decision != "approve" && req.Decision != "reject" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "decision must be 'approve' or 'reject'"})
		return
	}

	p, err := s.store.GetProposal(id)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	if p == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "no such approval"})
		return
	}
	if p.Status != "pending" {
		writeJSON(w, http.StatusConflict, map[string]any{
			"error": "already " + p.Status, "approval": s.toApproval(*p),
		})
		return
	}

	if s.decide == nil {
		writeJSON(w, http.StatusServiceUnavailable,
			map[string]any{"error": "no agent is available to act on this decision"})
		return
	}

	member := consoleUser(r).Member
	s.audit(r, "human", member, "console.approval."+req.Decision, p.Title, req.Decision, strings.TrimSpace(req.Note))

	if err := s.decide(id, req.Decision, req.Note, member); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}

	// Execution is asynchronous, so this returns the row as it stands now and
	// the console polls until it settles to executed or failed.
	updated, err := s.store.GetProposal(id)
	if err != nil || updated == nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "could not re-read the approval"})
		return
	}
	writeJSON(w, http.StatusOK, s.toApproval(*updated))
}
