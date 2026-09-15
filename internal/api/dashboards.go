package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/MelloB1989/karmax/internal/dashboards"
)

// Dashboards over HTTP: read-only plus delete and installing the component
// kit's reference doc. Writing a dashboard's HTML and data is the "dashboard"
// tool's job (see internal/tools/builtin/dashboard.go and its place in
// browserScopedTools) — these routes exist for the client that renders them,
// which holds the operator's own full token like every other route here, not
// a scoped one. Reading is left to either token; the two routes that change
// what another agent sees check scope like handleCallTool does.

// dashboardReferenceLimit bounds the PUT body for the kit's reference doc —
// generous for a Markdown file, small enough that a client mistake (or a
// malicious one, with a stolen full token) can't be used to fill the disk.
const dashboardReferenceLimit = 256 << 10

// requireFullScope refuses a request made with the harness's browser-scoped
// token. That token builds dashboards through the dashboard tool, which only
// writes the pages it is asked to; deleting anyone's dashboard, or rewriting
// the reference every agent reads before it writes a page, belongs to the
// operator's own token — otherwise one session could plant instructions for
// every session after it.
func requireFullScope(w http.ResponseWriter, r *http.Request) bool {
	if scopeFromContext(r.Context()) == scopeFull {
		return true
	}
	writeJSON(w, http.StatusForbidden, map[string]any{"error": "this token cannot change dashboards directly; use the dashboard tool"})
	return false
}

func (s *Server) handleListDashboards(w http.ResponseWriter, r *http.Request) {
	list, err := dashboards.List()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	if list == nil {
		list = []dashboards.Meta{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"dashboards": list})
}

func (s *Server) handleGetDashboard(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	meta, html, err := dashboards.Get(id)
	if err != nil {
		writeJSON(w, dashboardErrStatus(err), map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"dashboard": meta, "html": html})
}

func (s *Server) handleGetDashboardData(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	name := r.PathValue("name")
	b, err := dashboards.GetData(id, name)
	if err != nil {
		writeJSON(w, dashboardErrStatus(err), map[string]any{"error": err.Error()})
		return
	}
	// Written straight through rather than round-tripped through
	// json.Marshal(json.RawMessage(b)): it is already a JSON document on
	// disk, and re-encoding it could only change it, never validate it.
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(b)
}

// patchDashboardLimit bounds the PATCH body — it only ever carries two
// booleans, so anything past a few bytes is already a caller mistake, not a
// legitimate request growing.
const patchDashboardLimit = 4 << 10

// handlePatchDashboard sets pin/archive state. Full-scope only, like delete:
// pinning is shown first and archiving hides a tab, and both are the
// operator's call about their own dashboard list, not something an agent's
// harness token gets to do to itself.
func (s *Server) handlePatchDashboard(w http.ResponseWriter, r *http.Request) {
	if !requireFullScope(w, r) {
		return
	}
	id := r.PathValue("id")

	// Decoded as raw fields, not straight into a {Pinned, Archived *bool}
	// struct, because that shape can't tell "unknown key" from "field I don't
	// have" — and a client that misspells "archived" deserves a 400, not a
	// silent no-op.
	var raw map[string]json.RawMessage
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, patchDashboardLimit)).Decode(&raw); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "bad JSON body: " + err.Error()})
		return
	}
	if len(raw) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "nothing to change"})
		return
	}

	var pinned, archived *bool
	for key, v := range raw {
		var target **bool
		switch key {
		case "pinned":
			target = &pinned
		case "archived":
			target = &archived
		default:
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": fmt.Sprintf("unknown field %q", key)})
			return
		}
		var b bool
		if err := json.Unmarshal(v, &b); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": fmt.Sprintf("%q must be a bool", key)})
			return
		}
		*target = &b
	}

	meta, err := dashboards.SetFlags(id, pinned, archived)
	if err != nil {
		writeJSON(w, dashboardErrStatus(err), map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"dashboard": meta})
}

func (s *Server) handleDeleteDashboard(w http.ResponseWriter, r *http.Request) {
	if !requireFullScope(w, r) {
		return
	}
	id := r.PathValue("id")
	if err := dashboards.Delete(id); err != nil {
		writeJSON(w, dashboardErrStatus(err), map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": true})
}

func (s *Server) handlePutDashboardReference(w http.ResponseWriter, r *http.Request) {
	if !requireFullScope(w, r) {
		return
	}
	// Capped at the limit plus one byte: reading exactly the limit would
	// silently accept an oversized body that happens to end there, so the
	// check below needs proof there was more, not just that reading stopped.
	body, err := io.ReadAll(io.LimitReader(r.Body, dashboardReferenceLimit+1))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "read body: " + err.Error()})
		return
	}
	if len(body) > dashboardReferenceLimit {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "reference text is over the 256 KB limit"})
		return
	}
	if err := dashboards.WriteComponentReference(string(body)); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// dashboardErrStatus maps a dashboards package error to the status an API
// caller can act on: 400 for an id/name it sent that was never going to name
// anything, 404 for one that parses but names nothing on disk, 500 for
// everything else (a disk problem, not the caller's mistake).
func dashboardErrStatus(err error) int {
	switch {
	case errors.Is(err, dashboards.ErrInvalidID), errors.Is(err, dashboards.ErrInvalidName):
		return http.StatusBadRequest
	case errors.Is(err, dashboards.ErrNotFound):
		return http.StatusNotFound
	default:
		return http.StatusInternalServerError
	}
}
