package api

import (
	"net/http"
	"sort"
	"strings"

	"github.com/MelloB1989/karmax/internal/store"
)

// Settings: model providers, the sandbox token, the directory, and who holds
// which console role.
//
// No secret is ever returned in full. The console shows whether a key is set
// and its last four characters, which is enough to tell two keys apart and not
// enough to use one.

type modelProvider struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	BaseURL  string `json:"base_url"`
	HasKey   bool   `json:"has_key"`
	KeyLast4 string `json:"key_last4"`
}

type roleRow struct {
	Member string `json:"member"`
	Name   string `json:"name"`
	Role   string `json:"role"`
	Source string `json:"source"`
}

const sandboxTokenKey = "sandbox_agent_token"

func (s *ConsoleServer) handleSettings(w http.ResponseWriter, r *http.Request) {
	providers := []modelProvider{}
	if s.cfg != nil {
		ids := make([]string, 0, len(s.cfg.AI.Providers))
		for id := range s.cfg.AI.Providers {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		for _, id := range ids {
			p := s.cfg.AI.Providers[id]
			key := p.APIKey
			if key == "" {
				key = p.AuthToken
			}
			providers = append(providers, modelProvider{
				ID: id, Name: labelFor(id), BaseURL: p.BaseURL,
				HasKey: key != "", KeyLast4: last4(key),
			})
		}
	}

	sandbox := map[string]any{"configured": false, "last4": "", "updated_at": nil}
	if cred, err := s.store.Credential(sandboxTokenKey); err == nil && cred != nil && cred.AccessToken != "" {
		sandbox = map[string]any{
			"configured": true,
			"last4":      last4(cred.AccessToken),
			"updated_at": rfc3339Ptr(cred.UpdatedAt),
		}
	}

	members, err := s.store.ListDirectory("")
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	assigned, err := s.store.ConsoleRoleAssignments()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	accounts, err := s.store.ListConsoleUsers()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}

	// One row per member. The directory can list the same person from Slack and
	// from Jira, and showing them twice with two role dropdowns invites setting
	// one and not the other.
	rows := map[string]roleRow{}
	sources := map[string]bool{}
	for _, m := range members {
		sources[m.ExternalKind] = true
		if m.Member == "" {
			continue
		}
		if _, seen := rows[m.Member]; !seen {
			rows[m.Member] = roleRow{Member: m.Member, Name: m.Name, Role: "viewer", Source: "directory"}
		}
	}
	for member, role := range assigned {
		row, ok := rows[member]
		if !ok {
			row = roleRow{Member: member, Name: member, Source: "manual"}
		}
		row.Role = role
		rows[member] = row
	}
	// A console account is the authority on its own role.
	for _, a := range accounts {
		row, ok := rows[a.Member]
		if !ok {
			row = roleRow{Member: a.Member, Source: "manual"}
		}
		row.Role = a.Role
		if a.Name != "" {
			row.Name = a.Name
		}
		rows[a.Member] = row
	}

	roles := make([]roleRow, 0, len(rows))
	for _, r := range rows {
		roles = append(roles, r)
	}
	sort.Slice(roles, func(i, j int) bool { return roles[i].Member < roles[j].Member })

	kinds := make([]string, 0, len(sources))
	for k := range sources {
		kinds = append(kinds, k)
	}
	sort.Strings(kinds)

	var lastSync *string
	for _, m := range members {
		_ = m // the directory store records no sync timestamp; see below
		break
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"model_providers": providers,
		"sandbox_token":   sandbox,
		"directory": map[string]any{
			// null rather than a fabricated time: the directory table records no
			// sync timestamp, and inventing one would misreport how fresh it is.
			"last_synced_at": lastSync,
			"members_synced": len(rows),
			"sources":        kinds,
		},
		"roles": roles,
	})
}

func (s *ConsoleServer) handleSetModelProvider(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req struct {
		BaseURL string `json:"base_url"`
		APIKey  string `json:"api_key"`
	}
	if err := readJSON(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid JSON"})
		return
	}
	if s.cfg == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "no configuration is loaded"})
		return
	}
	p, ok := s.cfg.AI.Providers[id]
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "no such provider"})
		return
	}

	if strings.TrimSpace(req.BaseURL) != "" {
		p.BaseURL = strings.TrimSpace(req.BaseURL)
	}
	// A blank key means "keep the current one", matching the console's
	// leave-blank-to-keep field. Treating blank as "clear it" would delete a
	// working key every time someone edited the base URL.
	if strings.TrimSpace(req.APIKey) != "" {
		p.APIKey = strings.TrimSpace(req.APIKey)
	}
	s.cfg.AI.Providers[id] = p

	s.audit(r, "human", consoleUser(r).Member, "console.settings.model", id, "", "provider updated")
	// In-memory only: this server does not rewrite karmax.yaml, so the change
	// applies now and is lost on restart unless the file is edited too. Said
	// plainly rather than silently.
	writeJSON(w, http.StatusOK, map[string]any{
		"applied":    true,
		"persistent": false,
		"note":       "Applied to the running process. Edit karmax.yaml to make it survive a restart.",
	})
}

func (s *ConsoleServer) handleSetSandboxToken(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Token string `json:"token"`
	}
	if err := readJSON(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid JSON"})
		return
	}
	if strings.TrimSpace(req.Token) == "" {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"error": "a token is required"})
		return
	}

	if err := s.store.SaveCredential(store.Credential{
		Connector:   sandboxTokenKey,
		Config:      map[string]string{},
		AccessToken: strings.TrimSpace(req.Token),
		Enabled:     true,
	}); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}

	// The token itself is never echoed, not even the value just submitted.
	s.audit(r, "human", consoleUser(r).Member, "console.settings.sandbox_token", sandboxTokenKey, "", "token updated")
	writeJSON(w, http.StatusOK, map[string]any{"configured": true, "last4": last4(strings.TrimSpace(req.Token))})
}

func (s *ConsoleServer) handleDirectorySync(w http.ResponseWriter, r *http.Request) {
	if s.runDirectorySync == nil {
		writeJSON(w, http.StatusServiceUnavailable,
			map[string]any{"error": "no connector on this server can sync a directory"})
		return
	}

	n, sources, err := s.runDirectorySync(r.Context())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	if sources == nil {
		sources = []string{}
	}

	s.audit(r, "human", consoleUser(r).Member, "console.settings.directory_sync", "directory", "", "")
	writeJSON(w, http.StatusOK, map[string]any{
		"last_synced_at": rfc3339Ptr(nowUTC()),
		"members_synced": n,
		"sources":        sources,
	})
}

func (s *ConsoleServer) handleSetRole(w http.ResponseWriter, r *http.Request) {
	member := r.PathValue("member")
	var req struct {
		Role string `json:"role"`
	}
	if err := readJSON(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid JSON"})
		return
	}
	if !store.ValidConsoleRole(req.Role) {
		writeJSON(w, http.StatusUnprocessableEntity,
			map[string]any{"error": "role must be one of: " + strings.Join(store.ConsoleRoles, ", ")})
		return
	}

	// An admin must not be able to demote themselves out of the last admin
	// seat: the console would then have no one who can grant it back.
	if member == consoleUser(r).Member && req.Role != "admin" {
		accounts, err := s.store.ListConsoleUsers()
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		admins := 0
		for _, a := range accounts {
			if a.Role == "admin" {
				admins++
			}
		}
		if admins <= 1 {
			writeJSON(w, http.StatusConflict, map[string]any{
				"error": "you are the only admin — promote someone else before changing your own role",
			})
			return
		}
	}

	if err := s.store.SetConsoleRole(member, req.Role); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}

	s.audit(r, "human", consoleUser(r).Member, "console.settings.role", member, req.Role, "")
	writeJSON(w, http.StatusOK, roleRow{Member: member, Name: member, Role: req.Role, Source: "manual"})
}
