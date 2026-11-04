package desk

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/MelloB1989/karmax/fleet/internal/fleetapi"
)

// SnapEvery is how stale the list a long poll answers with may be: however
// many windows wait on the desk, it is looked at no more than once in this.
var SnapEvery = time.Second

// snapshot is the latest list without screens, shared by every waiter.
type snapshot struct {
	mu   sync.Mutex // held while one is made: the others wait for it
	list fleetapi.DeskList
	at   time.Time
	err  error

	kmu   sync.Mutex
	kick  chan struct{} // closed by touch: a change through the desk
	dirty bool
}

// touch says the desk changed through it: waiters look again now.
func (m *Manager) touch() {
	m.snap.kmu.Lock()
	defer m.snap.kmu.Unlock()
	m.snap.dirty = true
	if m.snap.kick != nil {
		close(m.snap.kick)
		m.snap.kick = nil
	}
}

func (m *Manager) kicked() <-chan struct{} {
	m.snap.kmu.Lock()
	defer m.snap.kmu.Unlock()
	if m.snap.kick == nil {
		m.snap.kick = make(chan struct{})
	}
	return m.snap.kick
}

// current is the shared list: made again when older than SnapEvery or
// after a change.
func (m *Manager) current(ctx context.Context) (fleetapi.DeskList, error) {
	m.snap.mu.Lock()
	defer m.snap.mu.Unlock()
	m.snap.kmu.Lock()
	dirty := m.snap.dirty
	m.snap.dirty = false
	m.snap.kmu.Unlock()
	if !dirty && !m.snap.at.IsZero() && m.Now().Sub(m.snap.at) < SnapEvery {
		return m.snap.list, m.snap.err
	}
	l, err := m.List(ctx, 0)
	if err == nil {
		l.Version = version(l)
	}
	m.snap.list, m.snap.err, m.snap.at = l, err, m.Now()
	return l, err
}

// version is a list's fingerprint: what a waiter compares.
func version(l fleetapi.DeskList) string {
	b, _ := json.Marshal(l)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:8])
}

// Changes is the list once it is no longer version since, or, after wait,
// as it is. since "" answers at once.
func (m *Manager) Changes(ctx context.Context, since string, wait time.Duration) (fleetapi.DeskList, error) {
	deadline := m.Now().Add(wait)
	for {
		kick := m.kicked() // before looking: a change after the look wakes us
		l, err := m.current(ctx)
		if err != nil || since == "" || l.Version != since || !m.Now().Before(deadline) {
			return l, err
		}
		t := time.NewTimer(min(SnapEvery, deadline.Sub(m.Now())))
		select {
		case <-ctx.Done():
			t.Stop()
			return l, nil
		case <-kick:
		case <-t.C:
		}
		t.Stop()
	}
}

// branchCache is each directory's git branch, looked at once in branchTTL.
type branchCache struct {
	mu sync.Mutex
	m  map[string]branchAt
}

type branchAt struct {
	branch string
	at     time.Time
}

const branchTTL = 5 * time.Second

func (c *branchCache) of(dir string, now time.Time) string {
	if dir == "" {
		return ""
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if b, ok := c.m[dir]; ok && now.Sub(b.at) < branchTTL {
		return b.branch
	}
	if c.m == nil || len(c.m) > 512 {
		c.m = map[string]branchAt{}
	}
	b := gitBranch(dir)
	c.m[dir] = branchAt{b, now}
	return b
}

// gitBranch is the branch checked out in the repository dir is in, a short
// commit when detached, or "".
func gitBranch(dir string) string {
	for d := dir; ; d = filepath.Dir(d) {
		g := filepath.Join(d, ".git")
		if fi, err := os.Stat(g); err == nil {
			if !fi.IsDir() { // a worktree or submodule: gitdir: <path>
				b, err := os.ReadFile(g)
				if err != nil {
					return ""
				}
				p, ok := strings.CutPrefix(strings.TrimSpace(string(b)), "gitdir: ")
				if !ok {
					return ""
				}
				if !filepath.IsAbs(p) {
					p = filepath.Join(d, p)
				}
				g = p
			}
			head, err := os.ReadFile(filepath.Join(g, "HEAD"))
			if err != nil {
				return ""
			}
			h := strings.TrimSpace(string(head))
			if ref, ok := strings.CutPrefix(h, "ref: refs/heads/"); ok {
				return ref
			}
			if len(h) >= 7 && !strings.HasPrefix(h, "ref:") {
				return h[:7]
			}
			return ""
		}
		if p := filepath.Dir(d); p == d {
			return ""
		}
	}
}
