package api

import (
	"encoding/json"
	"net/http"

	"github.com/MelloB1989/karmax/internal/fsscope"
)

// What the assistant may touch, over HTTP, so an app can ask the question.
//
// The app's job is to ask it in a way somebody can answer: which folders it
// works in, and what is never reachable. The standard denials are sent along
// with the operator's own so the app can show them — being able to see that
// your SSH keys are already out of reach is most of the reassurance.

type accessView struct {
	Enforced bool            `json:"enforced"`
	Grants   []fsscope.Grant `json:"grants"`
	Deny     []string        `json:"deny"`
	// Standard is the list KARMAX applies on its own. Read-only: it is here to
	// be shown, and the app should say so rather than offering a delete.
	Standard []string `json:"standard"`
}

func (s *Server) dataDir() string {
	if s.cfg == nil {
		return ""
	}
	return s.cfg.Karmax.DataDir
}

func (s *Server) handleGetAccess(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.accessView())
}

func (s *Server) accessView() accessView {
	p := fsscope.Load(s.dataDir())
	view := accessView{
		Enforced: p.Enforced,
		Grants:   p.Grants,
		Deny:     p.Deny,
		Standard: fsscope.StandardDenies(s.dataDir()),
	}
	if view.Grants == nil {
		view.Grants = []fsscope.Grant{}
	}
	if view.Deny == nil {
		view.Deny = []string{}
	}
	return view
}

// handlePutAccess replaces the policy.
//
// A whole policy rather than one change at a time: the screen this comes from
// shows the entire list, and sending back what is on it means the two cannot
// disagree about what was just removed.
func (s *Server) handlePutAccess(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Enforced bool `json:"enforced"`
		Grants   []struct {
			Path  string `json:"path"`
			Write bool   `json:"write"`
		} `json:"grants"`
		Deny []string `json:"deny"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&body); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	var p fsscope.Policy
	for _, g := range body.Grants {
		if err := p.Allow(g.Path, g.Write); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
	}
	for _, d := range body.Deny {
		if err := p.Forbid(d); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
	}
	// Allow and Forbid both turn enforcement on, so an empty policy that was
	// meant to be enforced still is — and one that was meant to be off is.
	p.Enforced = body.Enforced

	if err := fsscope.Save(s.dataDir(), p); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, s.accessView())
}
