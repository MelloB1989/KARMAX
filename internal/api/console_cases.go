package api

import (
	"net/http"
	"sort"
	"strings"

	"go.uber.org/zap"

	"github.com/MelloB1989/karmax/internal/store"
)

// Cases: a straight read-through of the case store. Nothing is computed here
// that the store does not already know.

type consoleCase struct {
	ID            string `json:"id"`
	Org           string `json:"org"`
	Agent         string `json:"agent"`
	Key           string `json:"key"`
	Title         string `json:"title"`
	State         string `json:"state"`
	Namespace     string `json:"namespace"`
	ThreadChannel string `json:"thread_channel"`
	ThreadTS      string `json:"thread_ts"`
	CreatedAt     string `json:"created_at"`
	UpdatedAt     string `json:"updated_at"`
}

func toConsoleCase(c store.Case) consoleCase {
	return consoleCase{
		ID: c.ID, Org: c.Org, Agent: c.Agent, Key: c.Key, Title: c.Title,
		State: c.State, Namespace: c.Namespace,
		ThreadChannel: c.ThreadChannel, ThreadTS: c.ThreadTS,
		CreatedAt: rfc3339(c.CreatedAt), UpdatedAt: rfc3339(c.UpdatedAt),
	}
}

// auditRow is one line of the audit view.
type auditRow struct {
	ID        string `json:"id"`
	ActorKind string `json:"actor_kind"`
	ActorID   string `json:"actor_id"`
	Agent     string `json:"agent"`
	CaseID    string `json:"case_id"`
	Recipe    string `json:"recipe"`
	Step      string `json:"step"`
	Verb      string `json:"verb"`
	Target    string `json:"target"`
	Decision  string `json:"decision"`
	Detail    string `json:"detail"`
	CreatedAt string `json:"created_at"`
}

type consoleCaseEvent struct {
	ID        string `json:"id"`
	CaseID    string `json:"case_id"`
	Kind      string `json:"kind"`
	Payload   string `json:"payload"`
	Actor     string `json:"actor"`
	CreatedAt string `json:"created_at"`
}

type consoleSandboxRun struct {
	ID          string  `json:"id"`
	CaseID      string  `json:"case_id"`
	Driver      string  `json:"driver"`
	ContainerID string  `json:"container_id"`
	Image       string  `json:"image"`
	Status      string  `json:"status"`
	Repo        string  `json:"repo"`
	Branch      string  `json:"branch"`
	Task        string  `json:"task"`
	StartedAt   string  `json:"started_at"`
	FinishedAt  *string `json:"finished_at"`
	ExitCode    int     `json:"exit_code"`
	Error       string  `json:"error"`
	LogTail     string  `json:"log_tail"`
}

func (s *ConsoleServer) handleCases(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	cases, err := s.store.ListCases(q.Get("agent"), q.Get("state"), queryInt(r, "limit", 50))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}

	// An empty list must serialise as [] and not null: the console maps over
	// this directly, and null is a crash rather than an empty table.
	out := make([]consoleCase, 0, len(cases))
	for _, c := range cases {
		out = append(out, toConsoleCase(c))
	}
	writeJSON(w, http.StatusOK, map[string]any{"cases": out})
}

func (s *ConsoleServer) handleCaseDetail(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	c, ok, err := s.store.CaseByID(id)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "no such case"})
		return
	}

	history, err := s.store.CaseHistory(id, 200)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	events := make([]consoleCaseEvent, 0, len(history))
	for _, e := range history {
		events = append(events, consoleCaseEvent{
			ID: e.ID, CaseID: e.CaseID, Kind: e.Kind, Payload: e.Payload,
			Actor: e.Actor, CreatedAt: rfc3339(e.CreatedAt),
		})
	}

	sandboxRuns, err := s.store.ListSandboxRuns(id, 50)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	runs := make([]consoleSandboxRun, 0, len(sandboxRuns))
	for _, sr := range sandboxRuns {
		var finished *string
		if sr.FinishedAt != nil {
			finished = rfc3339Ptr(*sr.FinishedAt)
		}
		runs = append(runs, consoleSandboxRun{
			ID: sr.ID, CaseID: sr.CaseID, Driver: sr.Driver, ContainerID: sr.ContainerID,
			Image: sr.Image, Status: sr.Status, Repo: sr.Repo, Branch: sr.Branch, Task: sr.Task,
			StartedAt: rfc3339(sr.StartedAt), FinishedAt: finished,
			ExitCode: sr.ExitCode, Error: sr.Error, LogTail: sr.LogTail,
		})
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"case": toConsoleCase(c), "events": events, "sandbox_runs": runs,
	})
}

// handleConsoleAudit answers with what this install actually recorded.
//
// Two sources, deliberately merged rather than one:
//
//   - audit_events, written when an agent makes a decision it is answerable
//     for. This is the real audit trail and always takes precedence.
//   - the durable event log, which is every webhook, scheduled job, agent turn
//     and loop run the daemon has processed.
//
// Only the org loop-kit writes audit_events today, so on an install that is
// not running org packs that table is EMPTY while the event log holds
// everything the system has done. Showing only the first would render a blank
// page on a busy server and imply nothing had happened, which is the most
// misleading thing an audit screen can do.
//
// Log-derived rows carry actor_kind "system" so the two are never confused for
// each other: a decision somebody is answerable for and a thing that happened
// are different claims.
func (s *ConsoleServer) handleConsoleAudit(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit := queryInt(r, "limit", 100)
	f := store.AuditFilter{
		ActorID:  q.Get("actor_id"),
		Agent:    q.Get("agent"),
		CaseID:   q.Get("case_id"),
		VerbLike: q.Get("verb"), // the console's filter box is free text
		Limit:    limit,
	}
	if since := q.Get("since"); since != "" {
		t, err := parseRFC3339(since)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "since must be an RFC3339 timestamp"})
			return
		}
		f.Since = t
	}

	events, err := s.store.QueryAudit(f)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}

	out := make([]auditRow, 0, limit)
	for _, e := range events {
		out = append(out, auditRow{
			ID: e.ID, ActorKind: e.ActorKind, ActorID: e.ActorID, Agent: e.Agent,
			CaseID: e.CaseID, Recipe: e.Recipe, Step: e.Step, Verb: e.Verb,
			Target: e.Target, Decision: e.Decision, Detail: e.Detail,
			CreatedAt: rfc3339(e.CreatedAt),
		})
	}

	// A case filter is a question about the case machinery, which the event log
	// knows nothing about; answering it from the log would be a non-answer
	// dressed as one.
	if len(out) < limit && f.CaseID == "" {
		out = append(out, s.logAsAudit(f, limit-len(out))...)
		sort.SliceStable(out, func(i, j int) bool { return out[i].CreatedAt > out[j].CreatedAt })
	}

	writeJSON(w, http.StatusOK, map[string]any{"events": out})
}

// logAsAudit renders durable-log entries in the audit shape.
func (s *ConsoleServer) logAsAudit(f store.AuditFilter, limit int) []auditRow {
	// Over-read, because the filters below are applied in Go: asking for
	// exactly `limit` rows and then discarding some would silently under-fill
	// the page.
	events, err := s.store.RecentLogEvents(store.DefaultWorkspace, "", limit*4)
	if err != nil {
		s.log.Warn("could not read the event log for the audit view", zap.Error(err))
		return nil
	}

	out := make([]auditRow, 0, limit)
	for _, e := range events {
		if len(out) >= limit {
			break
		}
		if f.Agent != "" && e.AgentID != f.Agent {
			continue
		}
		if f.ActorID != "" && e.AgentID != f.ActorID {
			continue
		}
		if f.VerbLike != "" && !strings.Contains(e.Kind, f.VerbLike) {
			continue
		}
		if !f.Since.IsZero() && e.CreatedAt.Before(f.Since) {
			continue
		}
		out = append(out, auditRow{
			ID:        e.ID,
			ActorKind: "system",
			ActorID:   e.AgentID,
			Agent:     e.AgentID,
			Verb:      e.Kind,
			Target:    logTarget(e),
			Detail:    logDetail(e),
			CreatedAt: rfc3339(e.CreatedAt),
		})
	}
	return out
}

// logTarget picks the field of a payload that names what the event was about.
func logTarget(e store.LogEvent) string {
	for _, key := range []string{"name", "loop", "recipe", "job", "channel", "path", "target"} {
		if v, ok := e.Payload[key]; ok {
			if str, ok := v.(string); ok && str != "" {
				return str
			}
		}
	}
	return ""
}

// truncate shortens a string to n characters.
//
// Counted in runes, not bytes: slicing a byte offset splits a multi-byte
// character in half and emits invalid UTF-8, which the JSON encoder then
// replaces with a corruption marker.
func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "\u2026"
}

// logDetail renders a one-line summary of a payload without dumping the whole
// thing into a table cell.
func logDetail(e store.LogEvent) string {
	for _, key := range []string{"summary", "detail", "message", "text", "status", "error"} {
		if v, ok := e.Payload[key]; ok {
			if str, ok := v.(string); ok && str != "" {
				return truncate(str, 240)
			}
		}
	}
	return ""
}
