package server

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/MelloB1989/karmax/fleet/internal/fleetapi"
)

// watchOps is what the watch stream asks of fleetd; *daemon.Fleet is the
// real one.
type watchOps interface {
	// Snapshot is the fleet as a caller sees it: everything when all, else
	// only the agents named and me's own tasks on them.
	Snapshot(now time.Time, all bool, agents []string, me fleetapi.Me) *fleetapi.Snapshot
	SnapshotSeq() uint64
	// Changed is closed when the next snapshot is made.
	Changed() <-chan struct{}
	// KnockCount is how many untagged devices asked for access.
	KnockCount() int
}

// watchWait is the longest a watch is held open before it answers with the
// snapshot it has.
var watchWait = 25 * time.Second

// watchRoutes serves GET /v1/watch?since=<seq>, the TUI's one stream: a long
// poll that answers at once when fleetd has a snapshot other than since (a
// newer one, or one from before fleetd restarted), else when the next is
// made, and after watchWait at most. Each answer is cut to the caller.
func watchRoutes(route func(string, func(http.ResponseWriter, *http.Request) (any, error), ...scope), long func(func(http.ResponseWriter, *http.Request) (any, error)) func(http.ResponseWriter, *http.Request) (any, error), ops Ops) {
	route("GET "+fleetapi.PathWatch, long(func(w http.ResponseWriter, r *http.Request) (any, error) {
		var since uint64
		if s := r.URL.Query().Get("since"); s != "" {
			var err error
			if since, err = strconv.ParseUint(s, 10, 64); err != nil {
				return nil, badRequest{fmt.Errorf("since %q: want the seq of the last snapshot, or 0", s)}
			}
		}
		timer := time.NewTimer(watchWait)
		defer timer.Stop()
		for {
			changed := ops.Changed() // before the seq: a change in between is not missed
			if ops.SnapshotSeq() != since {
				return snapshotFor(r, ops), nil
			}
			select {
			case <-changed:
			case <-timer.C:
				return snapshotFor(r, ops), nil
			case <-r.Context().Done():
				return nil, context.Cause(r.Context())
			}
		}
	}), and(readers, member)...)
}

// snapshotFor is the snapshot cut to the caller: an admin sees everything
// and the knocks; anyone else their lent agents and own tasks.
func snapshotFor(r *http.Request, ops Ops) *fleetapi.Snapshot {
	all, agents := lentAgents(r)
	s := ops.Snapshot(time.Now(), all, agents, me(r))
	if isAdmin(r) {
		s.Knocks = ops.KnockCount()
	}
	return s
}
