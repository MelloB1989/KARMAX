package daemon

import (
	"github.com/MelloB1989/karmax/fleet/internal/agent"
	"github.com/MelloB1989/karmax/fleet/internal/target"
	"github.com/MelloB1989/karmax/fleet/internal/term"
)

// Terminal is where a worker's session lives, for fleetctl ssh and the
// screen previews: its container through its host's exec prefix, and its
// slot's tmux session.
func (f *Fleet) Terminal(worker string) (term.Target, bool) {
	w, ok := f.cfg.Worker(worker)
	if !ok {
		return term.Target{}, false
	}
	return term.Target{Worker: w.Name, Host: w.Host, Pane: agent.PaneOf(w.Slot),
		Container: target.Container{Exec: f.cfg.Hosts[w.Host].Exec, Name: w.Container}}, true
}

// RecordTerm records a terminal session's start or end (fleet.term).
func (f *Fleet) RecordTerm(ev term.Event) {
	f.event(ev.Worker, "fleet.term", "", ev)
}
