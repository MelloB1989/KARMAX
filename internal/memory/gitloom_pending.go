package memory

import (
	"context"
	"strings"
	"sync"
	"time"

	gitloom "github.com/GitLoomHQ/gitloom-go/gitloom"
)

// Writes are asynchronous server-side, so a read right after a write can still
// return the old file or none. KARMAX keeps what it last wrote per path for a
// short while and folds onto that whenever the server has not caught up.

// pendingTTL bounds how long a written body is trusted over the server's.
const pendingTTL = 2 * time.Minute

type pendingWrite struct {
	stored gitloom.StoredMemory
	at     time.Time
}

// lockPath serialises read-modify-write on one path.
func (g *gitloomBackend) lockPath(path string) func() {
	g.pmu.Lock()
	l, ok := g.pathLocks[path]
	if !ok {
		l = &sync.Mutex{}
		g.pathLocks[path] = l
	}
	g.pmu.Unlock()
	l.Lock()
	return l.Unlock
}

// remember records a body that was just sent, dated by when sending began.
func (g *gitloomBackend) remember(m gitloom.NewMemory, occurred, sentAt time.Time) {
	g.pmu.Lock()
	defer g.pmu.Unlock()
	for p, w := range g.pending {
		if time.Since(w.at) > pendingTTL {
			delete(g.pending, p)
		}
	}
	g.pending[m.Path] = pendingWrite{at: sentAt, stored: gitloom.StoredMemory{
		Path: m.Path, Content: m.Content, UserTags: m.Tags, Cues: m.Cues, Related: m.Related,
		Confidence: m.Confidence, OccurredAt: occurred, UpdatedAt: sentAt,
	}}
}

func (g *gitloomBackend) dropPending(path string) {
	g.pmu.Lock()
	delete(g.pending, path)
	g.pmu.Unlock()
}

func (g *gitloomBackend) pendingFor(path string) (pendingWrite, bool) {
	g.pmu.Lock()
	defer g.pmu.Unlock()
	w, ok := g.pending[path]
	if ok && time.Since(w.at) > pendingTTL {
		delete(g.pending, path)
		return pendingWrite{}, false
	}
	return w, ok
}

// current is Get, reconciled with what KARMAX last wrote there. By content
// first: a server copy holding the local body is caught up, and one the local
// body extends is behind. Only a copy that is neither (another writer, or a
// rewrite) is settled by which was updated last.
func (g *gitloomBackend) current(ctx context.Context, path string) (*gitloom.StoredMemory, error) {
	existing, err := g.client.Get(ctx, path, &gitloom.RecallOptions{Namespace: g.cfg.Namespace})
	w, ok := g.pendingFor(path)
	if !ok {
		return existing, err
	}
	if err != nil {
		if !isAbsent(err) {
			return nil, err
		}
	} else if existing != nil {
		have, mine := strings.TrimSpace(existing.Content), strings.TrimSpace(w.stored.Content)
		switch {
		case strings.Contains(have, mine):
			return existing, nil
		case strings.Contains(mine, have):
		default:
			updated := firstTime(stamp(existing.UpdatedAt, existing.Updated), stamp(existing.CreatedAt, existing.Created))
			if updated.After(w.at) {
				return existing, nil
			}
		}
	}
	local := w.stored
	return &local, nil
}
