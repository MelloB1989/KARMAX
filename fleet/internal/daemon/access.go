package daemon

import (
	"context"
	"slices"
	"time"

	"github.com/MelloB1989/karmax/fleet/internal/access"
	"github.com/MelloB1989/karmax/fleet/internal/fleetapi"
	"github.com/MelloB1989/karmax/fleet/internal/identity"
	"github.com/MelloB1989/karmax/fleet/internal/tailnet"
)

// Device access (internal/access), kept in the ledger: the tailnet listener
// resolves each caller's device here, and fleetctl access changes the tags.

// Sandbox reports whether a worker's agent is a sandbox agent (sandbox: true
// in fleet.yaml): the only kind that can be lent, and never admin work.
func (f *Fleet) Sandbox(worker string) bool {
	w, ok := f.cfg.Worker(worker)
	return ok && w.Sandbox
}

// adminWork reports whether a task the full scope gives worker from `from`
// may use the operator's data: never on a sandbox, and never one given for a
// login (a person's request passed on is theirs, not the operator's).
func (f *Fleet) adminWork(worker, from string) bool {
	return !f.Sandbox(worker) && f.OperatorAssigner(from)
}

// ResolveDevice is the tagged device a tailnet caller is, role "" when it is
// not tagged. A tagged device is marked seen.
func (f *Fleet) ResolveDevice(w tailnet.Who, now time.Time) (fleetapi.Device, error) {
	d, err := access.Resolve(f.db, w)
	if err == nil && d.Role != "" {
		if err := f.db.Seen(d.StableID, now); err != nil {
			f.Log("ledger: device seen: %v", err)
		}
	}
	return d, err
}

// Knocked records an untagged device's request, and tells the operator the
// first time in a day.
func (f *Fleet) Knocked(w tailnet.Who, now time.Time) error {
	return access.Knocked(f.db, w, now, func(text string) {
		f.event("", "fleet.access.knock", "", map[string]string{"device": w.Node, "login": w.Login, "stable_id": w.StableID})
		if f.notify == nil {
			return
		}
		go func() { // not on the refused request's time
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			if err := f.notify.Notify(ctx, text); err != nil {
				f.Log("knock %s: %v", w.Node, err)
			}
		}()
	})
}

// AccessList is the tagged devices and recent knocks.
func (f *Fleet) AccessList() (fleetapi.AccessList, error) { return access.List(f.db) }

// ChangeAccess tags, lends, unlends or revokes, as `by`, and records it.
func (f *Fleet) ChangeAccess(c fleetapi.AccessChange, by string, now time.Time) (fleetapi.AccessList, error) {
	l, err := access.Change(f.db, c, by, f.Sandbox, now)
	if err != nil {
		return l, err
	}
	f.event("", "fleet.access."+c.Op, "", map[string]any{"device": c.Device, "agents": c.Agents, "by": by})
	return l, nil
}

// KnockCount is how many untagged devices have asked: the TUI's badge.
func (f *Fleet) KnockCount() int {
	l, err := access.List(f.db)
	if err != nil {
		return 0
	}
	return len(l.Knocks)
}

// PickAmong is Pick limited to some agents (a guest's lent ones): the best
// eligible worker of theirs, and only their candidates, so a guest learns
// nothing of other agents.
func (f *Fleet) PickAmong(repo string, among []string, now time.Time) PickReply {
	return among1(f.PickFor(repo, "", "", now), among)
}

// among1 cuts a pick to the workers of some agents and chooses the best of
// those: the candidates are in rank order already.
func among1(p PickReply, among []string) PickReply {
	out := PickReply{Candidates: []Candidate{}}
	for _, c := range p.Candidates {
		if !slices.Contains(among, identity.Base(c.Agent)) {
			continue
		}
		if c.Eligible && out.Agent == "" {
			out.Agent, out.Why = c.Agent, "the best of your agents"
			c.Why = out.Why
		} else if c.Eligible {
			c.Why = "eligible, ranked below " + out.Agent
		}
		out.Candidates = append(out.Candidates, c)
	}
	return out
}
