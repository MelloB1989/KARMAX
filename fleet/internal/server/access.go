package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"time"

	"github.com/MelloB1989/karmax/fleet/internal/access"
	"github.com/MelloB1989/karmax/fleet/internal/daemon"
	"github.com/MelloB1989/karmax/fleet/internal/fleetapi"
	"github.com/MelloB1989/karmax/fleet/internal/identity"
	"github.com/MelloB1989/karmax/fleet/internal/ledger"
	"github.com/MelloB1989/karmax/fleet/internal/tailnet"
)

// AccessOps is device access as the API drives it; *daemon.Fleet is the
// real one.
type AccessOps interface {
	AccessList() (fleetapi.AccessList, error)
	ChangeAccess(c fleetapi.AccessChange, by string, now time.Time) (fleetapi.AccessList, error)
	// KnockCount is how many untagged devices asked: an admin's badge.
	KnockCount() int
	// Sandbox reports whether a worker's agent is a sandbox agent.
	Sandbox(worker string) bool
	// PickAmong is Pick among some agents only: a guest's lent ones.
	PickAmong(repo string, among []string, now time.Time) daemon.PickReply
}

// Devices is who a tailnet caller's device is, for Tailnet; *daemon.Fleet
// is the real one.
type Devices interface {
	// ResolveDevice is the device's tag: role "" when it has none.
	ResolveDevice(w tailnet.Who, now time.Time) (fleetapi.Device, error)
	// Knocked records an untagged device's request.
	Knocked(w tailnet.Who, now time.Time) error
}

// accessRoutes are fleetctl access: the tagged devices and knocks, and a
// change to them. Admins only.
func accessRoutes(route func(string, func(http.ResponseWriter, *http.Request) (any, error), ...scope), ops Ops) {
	route("GET "+fleetapi.PathAccess, func(w http.ResponseWriter, r *http.Request) (any, error) {
		return ops.AccessList()
	}, fullOnly...)
	route("POST "+fleetapi.PathAccess, func(w http.ResponseWriter, r *http.Request) (any, error) {
		var in fleetapi.AccessChange
		if err := body(r, &in); err != nil {
			return nil, err
		}
		by := ledger.Operator
		if c := callerOf(r); c != nil {
			by = c.Login + " (" + c.Device + ")"
		}
		l, err := ops.ChangeAccess(in, by, time.Now())
		switch {
		case errors.Is(err, access.ErrNoDevice):
			return nil, notFound{err}
		case errors.Is(err, access.ErrBadChange):
			return nil, badRequest{err}
		}
		return l, err
	}, fullOnly...)
}

// lentRows is the status rows of the lent agents' workers.
func lentRows(rows []daemon.Row, lent []string) []daemon.Row {
	out := []daemon.Row{}
	for _, row := range rows {
		if slices.Contains(lent, identity.Base(row.Agent)) {
			out = append(out, row)
		}
	}
	return out
}

// guestEvents is the events a guest may see: its lent agents', except those
// about a task that is not its own (an agent may be lent to several
// devices). Fleet-wide events (no agent) are not theirs.
func guestEvents(r *http.Request, ops Ops, evs []ledger.Event) []ledger.Event {
	_, lent := lentAgents(r)
	mine := map[string]bool{}
	out := []ledger.Event{}
	for _, e := range evs {
		if e.Agent == "" || !slices.Contains(lent, identity.Base(e.Agent)) {
			continue
		}
		var d struct {
			Task string `json:"task"`
		}
		_ = json.Unmarshal([]byte(e.Detail), &d)
		if d.Task != "" {
			ok, seen := mine[d.Task]
			if !seen {
				ok = own(r, ops, d.Task) == nil
				mine[d.Task] = ok
			}
			if !ok {
				continue
			}
		}
		out = append(out, e)
	}
	return out
}

// refuseDevice is the answer to a device with no tag: who fleetd sees, and
// what the operator runs to let it in. The knock is recorded.
func refuseDevice(w http.ResponseWriter, devices Devices, wh tailnet.Who) {
	if err := devices.Knocked(wh, time.Now()); err != nil {
		writeErr(w, http.StatusServiceUnavailable, fmt.Errorf("fleetd cannot read its access list: %v", err))
		return
	}
	writeErr(w, http.StatusForbidden, errors.New(access.Refusal(wh)))
}
