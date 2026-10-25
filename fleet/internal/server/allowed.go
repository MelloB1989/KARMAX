package server

import (
	"net/http"

	"github.com/MelloB1989/karmax/fleet/internal/fleetapi"
)

// Who may touch which agent. The routes the TUI work adds (watch, screens,
// term) ask these and never the scopes directly; the access work gives them
// their final rules: an admin device is the full scope, and a guest may use
// exactly the agents lent to its device.

// isAdmin is the full token (and, with device access, an admin device).
func isAdmin(r *http.Request) bool { return scopeOf(r) == full }

// mayUseAgent reports whether the caller may act on worker (an agent, or a
// slot of one): run on it, watch it, type in it, copy files with it.
func mayUseAgent(r *http.Request, worker string) bool {
	switch scopeOf(r) {
	case full, console:
		return true
	}
	return false // a guest: when identity.Base(worker) is lent to its device
}

// lentAgents is the agents a caller may see: all, or a guest's lent ones.
func lentAgents(r *http.Request) (all bool, agents []string) {
	switch scopeOf(r) {
	case full, console:
		return true, nil
	}
	return false, nil
}

// me is the caller as the watch snapshot names it.
func me(r *http.Request) fleetapi.Me {
	if c := callerOf(r); c != nil {
		m := fleetapi.Me{Name: c.Login, Role: fleetapi.RoleGuest}
		if isAdmin(r) {
			m.Role = fleetapi.RoleAdmin
		}
		_, m.Lent = lentAgents(r)
		return m
	}
	return fleetapi.Me{Name: "operator", Role: fleetapi.RoleAdmin}
}
