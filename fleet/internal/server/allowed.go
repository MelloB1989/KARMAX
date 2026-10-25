package server

import (
	"net/http"
	"slices"

	"github.com/MelloB1989/karmax/fleet/internal/fleetapi"
	"github.com/MelloB1989/karmax/fleet/internal/identity"
)

// Who may touch which agent. Every route that names or picks an agent asks
// these, never the scopes directly: an admin device is the full scope (the
// Caller's role sets it), and a guest may use exactly the agents lent to its
// device, as its tag said when the request came.

// isAdmin is the full token, or an admin device.
func isAdmin(r *http.Request) bool { return scopeOf(r) == full }

// mayUseAgent reports whether the caller may act on worker (an agent, or a
// slot of one): run on it, watch it, type in it, copy files with it.
func mayUseAgent(r *http.Request, worker string) bool {
	switch scopeOf(r) {
	case full, console:
		return true
	case guest:
		c := callerOf(r)
		return c != nil && worker != "" && slices.Contains(c.Lent, identity.Base(worker))
	}
	return false
}

// lentAgents is the agents a caller may see: all, or a guest's lent ones
// (none for any other scope).
func lentAgents(r *http.Request) (all bool, agents []string) {
	switch scopeOf(r) {
	case full, console:
		return true, nil
	case guest:
		if c := callerOf(r); c != nil {
			return false, slices.Clone(c.Lent)
		}
	}
	return false, nil
}

// me is the caller as the watch snapshot names it.
func me(r *http.Request) fleetapi.Me {
	if c := callerOf(r); c != nil {
		m := fleetapi.Me{Name: c.Login, Role: fleetapi.RoleGuest, Device: c.Device}
		if isAdmin(r) {
			m.Role = fleetapi.RoleAdmin
		} else {
			_, m.Lent = lentAgents(r)
		}
		return m
	}
	switch scopeOf(r) {
	case full, console:
		return fleetapi.Me{Name: "operator", Role: fleetapi.RoleAdmin}
	}
	return fleetapi.Me{Name: string(scopeOf(r)), Role: fleetapi.RoleGuest} // sees nothing: no lends
}
