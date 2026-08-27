package api

import (
	"net/http"
	"strings"

	"github.com/MelloB1989/karmax/internal/store"
)

// The organisation the agents work for.
//
// Readable by anyone signed in — an agent's standing context is not a secret
// from the people it works alongside — and writable by admins, because it is
// injected into every agent's prompt and is therefore a way to change how every
// agent behaves.

type orgProfileBody struct {
	Name        string `json:"name"`
	Domain      string `json:"domain"`
	Description string `json:"description"`
	Timezone    string `json:"timezone"`
	Context     string `json:"context"`
	UpdatedAt   string `json:"updated_at"`
	UpdatedBy   string `json:"updated_by"`
	// Briefing is exactly what the agents are given, rendered server-side, so
	// whoever writes the context can see the result rather than guess at it.
	Briefing string `json:"briefing"`
}

// orgContextLimit caps the context field.
//
// It goes into EVERY turn's prompt, so its length is a running cost paid on
// every message, not a one-off. Large enough for the things a new hire needs,
// small enough that nobody pastes a handbook in.
const orgContextLimit = 8000

func (s *ConsoleServer) handleGetOrg(w http.ResponseWriter, r *http.Request) {
	p, err := s.store.OrgProfileFor(store.DefaultOrg)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, orgProfileBody{
		Name: p.Name, Domain: p.Domain, Description: p.Description,
		Timezone: p.Timezone, Context: p.Context,
		UpdatedAt: rfc3339(p.UpdatedAt), UpdatedBy: p.UpdatedBy,
		Briefing: p.Briefing(),
	})
}

func (s *ConsoleServer) handleSetOrg(w http.ResponseWriter, r *http.Request) {
	var req orgProfileBody
	if err := readJSON(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid JSON"})
		return
	}
	if n := len([]rune(req.Context)); n > orgContextLimit {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
			"error": "the context is too long — it is added to every message the agents handle, " +
				"so it is capped at 8,000 characters; this is " + itoa(n),
		})
		return
	}

	me := consoleUser(r).Member
	p := store.OrgProfile{
		Org: store.DefaultOrg, Name: req.Name, Domain: req.Domain,
		Description: req.Description, Timezone: req.Timezone,
		Context: req.Context, UpdatedBy: me,
	}
	if err := s.store.SaveOrgProfile(p); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}

	// What changed, not what it now says: an audit line should not carry 8,000
	// characters of context every time someone fixes a typo.
	s.audit(r, "human", me, "console.org.update", store.DefaultOrg, "",
		"organisation profile updated"+contextNote(req.Context))

	saved, _ := s.store.OrgProfileFor(store.DefaultOrg)
	writeJSON(w, http.StatusOK, orgProfileBody{
		Name: saved.Name, Domain: saved.Domain, Description: saved.Description,
		Timezone: saved.Timezone, Context: saved.Context,
		UpdatedAt: rfc3339(saved.UpdatedAt), UpdatedBy: saved.UpdatedBy,
		Briefing: saved.Briefing(),
	})
}

func contextNote(c string) string {
	if strings.TrimSpace(c) == "" {
		return " (context cleared)"
	}
	return " (context: " + itoa(len([]rune(c))) + " characters)"
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
