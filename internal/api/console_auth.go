package api

import (
	"context"
	"net/http"
	"strings"

	"github.com/MelloB1989/karmax/internal/store"
	"go.uber.org/zap"
)

// Console authentication.
//
// Separate from Server.auth, which checks a single shared KARMAX_API_TOKEN for
// the phone app. The console has named humans with roles, so "who did this"
// has an answer in the audit log — one shared secret cannot tell two operators
// apart.

type consoleCtxKey struct{}

// consoleUser returns the authenticated operator on a request.
func consoleUser(r *http.Request) store.ConsoleUser {
	u, _ := r.Context().Value(consoleCtxKey{}).(store.ConsoleUser)
	return u
}

// session requires a valid console session.
func (s *ConsoleServer) session(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		header := r.Header.Get("Authorization")
		if !strings.HasPrefix(header, "Bearer ") {
			writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "unauthorized"})
			return
		}
		token := strings.TrimSpace(strings.TrimPrefix(header, "Bearer "))

		u, ok, err := s.store.ConsoleSessionUser(token)
		if err != nil {
			s.log.Error("console session lookup failed", zap.Error(err))
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "session lookup failed"})
			return
		}
		if !ok {
			writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "session expired"})
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), consoleCtxKey{}, u)))
	}
}

// role requires a session AND at least the named role.
//
// Read is open to any signed-in operator; anything that writes, spends, or
// changes what an agent may do requires more. A viewer who can approve a PR
// merge is not a viewer.
func (s *ConsoleServer) role(min string, next http.HandlerFunc) http.HandlerFunc {
	return s.session(func(w http.ResponseWriter, r *http.Request) {
		if !roleAtLeast(consoleUser(r).Role, min) {
			writeJSON(w, http.StatusForbidden,
				map[string]any{"error": "this action requires the " + min + " role"})
			return
		}
		next(w, r)
	})
}

// roleAtLeast reports whether have is at least as strong as want.
func roleAtLeast(have, want string) bool {
	rank := func(r string) int {
		for i, name := range store.ConsoleRoles {
			if name == r {
				return i
			}
		}
		return -1 // unknown role gets nothing, rather than defaulting to viewer
	}
	h, w := rank(have), rank(want)
	return h >= 0 && w >= 0 && h >= w
}

type sessionResponse struct {
	Token  string `json:"token"`
	Member string `json:"member"`
	Name   string `json:"name"`
	Role   string `json:"role"`
}

func (s *ConsoleServer) handleBootstrapStatus(w http.ResponseWriter, r *http.Request) {
	n, err := s.store.CountConsoleUsers()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"needs_bootstrap": n == 0})
}

func (s *ConsoleServer) handleBootstrap(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name     string `json:"name"`
		Member   string `json:"member"`
		Password string `json:"password"`
	}
	if err := readJSON(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid JSON"})
		return
	}

	// Re-check server-side. The client showing a bootstrap form proves nothing
	// about whether an admin exists.
	n, err := s.store.CountConsoleUsers()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	if n > 0 {
		writeJSON(w, http.StatusConflict, map[string]any{"error": "this console has already been set up"})
		return
	}

	u, err := s.store.CreateConsoleUser(req.Member, req.Name, "admin", req.Password)
	if err != nil {
		if err == store.ErrConsoleUserExists {
			writeJSON(w, http.StatusConflict, map[string]any{"error": "this console has already been set up"})
			return
		}
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	s.log.Info("console bootstrapped", zap.String("member", u.Member))
	s.audit(r, "human", u.Member, "console.bootstrap", u.Member, "", "first admin created")
	s.issueSession(w, u)
}

func (s *ConsoleServer) handleLogin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Member   string `json:"member"`
		Password string `json:"password"`
	}
	if err := readJSON(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid JSON"})
		return
	}

	u, err := s.store.AuthenticateConsoleUser(req.Member, req.Password)
	if err != nil {
		s.log.Warn("console login rejected", zap.String("member", req.Member))
		s.audit(r, "human", req.Member, "console.login.rejected", req.Member, "denied", "")
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "invalid credentials"})
		return
	}
	s.audit(r, "human", u.Member, "console.login", u.Member, "allowed", "")
	s.issueSession(w, u)
}

func (s *ConsoleServer) issueSession(w http.ResponseWriter, u store.ConsoleUser) {
	sess, err := s.store.CreateConsoleSession(u.Member, s.sessionTTL)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, sessionResponse{
		Token: sess.Token, Member: u.Member, Name: u.Name, Role: u.Role,
	})
}

func (s *ConsoleServer) handleMe(w http.ResponseWriter, r *http.Request) {
	u := consoleUser(r)
	// The token is not echoed back: the client already has it, and repeating a
	// credential in a response body is how it ends up in a log.
	writeJSON(w, http.StatusOK, sessionResponse{Member: u.Member, Name: u.Name, Role: u.Role})
}

func (s *ConsoleServer) handleLogout(w http.ResponseWriter, r *http.Request) {
	token := strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
	if err := s.store.DeleteConsoleSession(token); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{})
}

// audit records a console action. Failures are logged and swallowed: an audit
// write that fails must not also fail the operator's request, but it must not
// pass unnoticed either.
func (s *ConsoleServer) audit(r *http.Request, actorKind, actorID, verb, target, decision, detail string) {
	ev := store.AuditEvent{
		ActorKind: actorKind,
		ActorID:   actorID,
		Verb:      verb,
		Target:    target,
		Decision:  decision,
		Detail:    detail,
	}
	if err := s.store.AppendAudit(ev); err != nil {
		s.log.Error("failed to record console audit event",
			zap.String("verb", verb), zap.Error(err))
	}
}
