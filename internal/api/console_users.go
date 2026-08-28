package api

import (
	"net/http"
	"strings"

	"github.com/MelloB1989/karmax/internal/store"
)

// Console accounts: the people who can sign in.
//
// Admin-only throughout, except changing your OWN password. Handing out console
// access is handing out the ability to approve actions and read the org's
// memory, so it is not an operator-level decision.
//
// One rule runs through all of it: the last admin cannot be removed, demoted,
// or locked out. An install with no admin has nobody who can appoint one, and
// the only way back is editing the database by hand.

type consoleUserRow struct {
	Member string `json:"member"`
	Name   string `json:"name"`
	Role   string `json:"role"`
	// Email is the address a Google sign-in matches against. Empty means this
	// account can only be opened with a password.
	Email string `json:"email"`
	Self  bool   `json:"self"`
}

func (s *ConsoleServer) handleListUsers(w http.ResponseWriter, r *http.Request) {
	users, err := s.store.ListConsoleUsers()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	me := consoleUser(r).Member

	out := make([]consoleUserRow, 0, len(users))
	for _, u := range users {
		// No password hash, ever — not even its shape. This list is read by a
		// browser and copied into support threads.
		out = append(out, consoleUserRow{Member: u.Member, Name: u.Name, Role: u.Role, Email: u.Email, Self: u.Member == me})
	}
	writeJSON(w, http.StatusOK, map[string]any{"users": out, "roles": store.ConsoleRoles})
}

func (s *ConsoleServer) handleCreateUser(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Member   string `json:"member"`
		Name     string `json:"name"`
		Role     string `json:"role"`
		Password string `json:"password"`
		Email    string `json:"email"`
	}
	if err := readJSON(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid JSON"})
		return
	}
	if req.Role == "" {
		req.Role = "viewer"
	}
	if !store.ValidConsoleRole(req.Role) {
		writeJSON(w, http.StatusUnprocessableEntity,
			map[string]any{"error": "role must be one of: " + strings.Join(store.ConsoleRoles, ", ")})
		return
	}

	// An account may have a password, a Google address, or both — but not
	// neither, or it would exist with no way in at all.
	if strings.TrimSpace(req.Password) == "" && strings.TrimSpace(req.Email) == "" {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
			"error": "give the account a password, a Google sign-in address, or both — " +
				"otherwise there is no way to sign into it",
		})
		return
	}

	u, err := s.store.CreateConsoleUserWithEmail(req.Member, req.Name, req.Role, req.Password, req.Email)
	if err != nil {
		if err == store.ErrConsoleUserExists {
			writeJSON(w, http.StatusConflict, map[string]any{"error": "that member already has an account"})
			return
		}
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"error": err.Error()})
		return
	}

	s.audit(r, "human", consoleUser(r).Member, "console.user.create", u.Member, u.Role, "")
	writeJSON(w, http.StatusOK, consoleUserRow{Member: u.Member, Name: u.Name, Role: u.Role, Email: req.Email})
}

func (s *ConsoleServer) handleUpdateUser(w http.ResponseWriter, r *http.Request) {
	member := r.PathValue("member")
	var req struct {
		Name  string  `json:"name"`
		Role  string  `json:"role"`
		Email *string `json:"email"`
	}
	if err := readJSON(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid JSON"})
		return
	}

	existing, ok, err := s.store.ConsoleUserByMember(member)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "no such user"})
		return
	}

	if req.Role != "" && req.Role != existing.Role {
		if !store.ValidConsoleRole(req.Role) {
			writeJSON(w, http.StatusUnprocessableEntity,
				map[string]any{"error": "role must be one of: " + strings.Join(store.ConsoleRoles, ", ")})
			return
		}
		if existing.Role == "admin" && req.Role != "admin" {
			if blocked, msg := s.wouldStrandTheInstall(); blocked {
				writeJSON(w, http.StatusConflict, map[string]any{"error": msg})
				return
			}
		}
		if err := s.store.SetConsoleRole(member, req.Role); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		s.audit(r, "human", consoleUser(r).Member, "console.user.role", member, req.Role, "")
	}

	if strings.TrimSpace(req.Name) != "" && req.Name != existing.Name {
		if err := s.store.UpdateConsoleUser(member, req.Name); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
	}

	// A pointer, so "" clears the address and omitting the field leaves it
	// alone. Without that distinction there would be no way to remove somebody's
	// Google sign-in without also removing their name.
	if req.Email != nil {
		if err := s.store.SetConsoleUserEmail(member, *req.Email); err != nil {
			writeJSON(w, http.StatusConflict, map[string]any{"error": err.Error()})
			return
		}
		s.audit(r, "human", consoleUser(r).Member, "console.user.email", member, "", *req.Email)
	}

	updated, _, _ := s.store.ConsoleUserByMember(member)
	writeJSON(w, http.StatusOK, consoleUserRow{
		Member: updated.Member, Name: updated.Name, Role: updated.Role, Email: updated.Email,
		Self: member == consoleUser(r).Member,
	})
}

func (s *ConsoleServer) handleDeleteUser(w http.ResponseWriter, r *http.Request) {
	member := r.PathValue("member")

	existing, ok, err := s.store.ConsoleUserByMember(member)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "no such user"})
		return
	}
	if existing.Role == "admin" {
		if blocked, msg := s.wouldStrandTheInstall(); blocked {
			writeJSON(w, http.StatusConflict, map[string]any{"error": msg})
			return
		}
	}

	if err := s.store.DeleteConsoleUser(member); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	s.audit(r, "human", consoleUser(r).Member, "console.user.delete", member, "", "")
	writeJSON(w, http.StatusOK, map[string]any{"deleted": true, "member": member})
}

// handleSetPassword changes a password.
//
// Anyone may change their OWN — which is also the answer to an install having
// had no password-change route at all, so whatever was typed at bootstrap stood
// forever. Changing somebody else's is an admin action, and requires knowing
// the current one when it is your own.
func (s *ConsoleServer) handleSetPassword(w http.ResponseWriter, r *http.Request) {
	member := r.PathValue("member")
	me := consoleUser(r)

	if member != me.Member && !roleAtLeast(me.Role, "admin") {
		writeJSON(w, http.StatusForbidden,
			map[string]any{"error": "changing someone else's password requires the admin role"})
		return
	}

	var req struct {
		Current  string `json:"current_password"`
		Password string `json:"password"`
	}
	if err := readJSON(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid JSON"})
		return
	}

	// Proving you know the current one stops a walk-up at an unlocked laptop
	// from taking the account. An admin resetting somebody else's cannot know
	// it, and is already trusted with the role.
	if member == me.Member {
		if _, err := s.store.AuthenticateConsoleUser(member, req.Current); err != nil {
			writeJSON(w, http.StatusForbidden, map[string]any{"error": "the current password is wrong"})
			return
		}
	}

	if err := s.store.SetConsolePassword(member, req.Password); err != nil {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"error": err.Error()})
		return
	}

	s.audit(r, "human", me.Member, "console.user.password", member, "", "")
	// Every session for that account is gone, including this one if they
	// changed their own — say so rather than letting the console look broken.
	writeJSON(w, http.StatusOK, map[string]any{
		"changed":          true,
		"sessions_revoked": true,
		"sign_in_again":    member == me.Member,
	})
}

// wouldStrandTheInstall reports whether removing or demoting an admin would
// leave nobody able to appoint one.
func (s *ConsoleServer) wouldStrandTheInstall() (bool, string) {
	n, err := s.store.CountConsoleAdmins()
	if err != nil {
		return true, "could not count the remaining admins, so this was not attempted"
	}
	if n <= 1 {
		return true, "this is the only admin — appoint another one first, or the console " +
			"would be left with nobody who can grant the role back"
	}
	return false, ""
}
