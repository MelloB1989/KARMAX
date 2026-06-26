package api

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/MelloB1989/karmax/internal/store"
)

func normalizeContactPhone(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// handleSyncContacts receives the phone's contact directory from the app and
// stores it, so KARMAX can resolve WhatsApp numbers to saved names.
func (s *Server) handleSyncContacts(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Contacts []struct {
			Name   string   `json:"name"`
			Phones []string `json:"phones"`
		} `json:"contacts"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid json"})
		return
	}
	var list []store.Contact
	for _, c := range req.Contacts {
		name := strings.TrimSpace(c.Name)
		if name == "" {
			continue
		}
		for _, p := range c.Phones {
			ph := normalizeContactPhone(p)
			if len(ph) >= 7 {
				list = append(list, store.Contact{Phone: ph, Name: name})
			}
		}
	}
	n, err := s.store.UpsertContacts(list)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"synced": n, "total": s.store.CountContacts()})
}

func (s *Server) handleGetContacts(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"count": s.store.CountContacts()})
}
