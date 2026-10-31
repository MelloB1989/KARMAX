package daemon

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/MelloB1989/karmax/fleet/internal/archive"
	"github.com/MelloB1989/karmax/fleet/internal/cleanup"
	"github.com/MelloB1989/karmax/fleet/internal/fleetapi"
	"github.com/MelloB1989/karmax/fleet/internal/ledger"
	"github.com/MelloB1989/karmax/fleet/internal/reconcile"
)

// The cleanup pass (fleet.yaml cleanup): what finished work left behind,
// deleted once nothing used it for cleanup.stale. internal/cleanup's script
// does the deleting, in each agent's container and, for the shared code's
// worktrees, here on fleetd's machine; session archives are fleetd's own.
// Work found nowhere else (changes not committed, commits no remote has) is
// never deleted: it is kept, and you are told once.

const (
	// cleanupSettle is how long after fleetd starts its first pass waits: a
	// restart brings every agent back at once.
	cleanupSettle = 15 * time.Minute
	// cleanupTimeout bounds a pass, and cleanupOne one place's part of it.
	cleanupTimeout = time.Hour
	cleanupOne     = 20 * time.Minute
)

// Places a pass reports besides agents.
const (
	cleanCode    = "code"    // the shared code's .worktrees/<agent>
	cleanArchive = "archive" // fleetd's session archives
)

// Cleanup runs a pass now and reports it; dry says what would go and
// deletes nothing. One pass runs at a time: while one does, ErrConflict.
func (f *Fleet) Cleanup(ctx context.Context, dry bool, now time.Time) (fleetapi.CleanupReport, error) {
	if !f.cleanMu.TryLock() {
		return fleetapi.CleanupReport{}, fmt.Errorf("%w: a cleanup pass is running; ask again when it is done", ErrConflict)
	}
	defer f.cleanMu.Unlock()
	// The pass is the fleet's, not the caller's: it runs to the end.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
	defer cancel()
	return f.cleanup(ctx, dry, now), nil
}

// scheduleCleanup starts a pass when one is due (Tick). Only fleetd runs
// them on its own (Started is fleetd's).
func (f *Fleet) scheduleCleanup(ctx context.Context, now time.Time) {
	c := f.cfg.Cleanup
	if !c.On() || f.Started.IsZero() || now.Sub(f.Started) < cleanupSettle || now.Sub(f.lastCleanup) < c.Every {
		return
	}
	if !f.cleanMu.TryLock() {
		return // fleetctl cleanup is running one
	}
	f.lastCleanup = now
	f.cleanWG.Add(1)
	go func() {
		defer f.cleanWG.Done()
		defer f.cleanMu.Unlock()
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
		defer cancel()
		f.cleanup(cctx, false, now)
	}()
}

// lastCleanupAt is when the last pass that deleted ran, as the ledger has it.
func lastCleanupAt(db *ledger.DB) time.Time {
	evs, err := db.EventsWhere(ledger.EventQuery{Kinds: []string{"fleet.cleanup"}, Limit: 1, Newest: true})
	if err != nil || len(evs) == 0 {
		return time.Time{}
	}
	return evs[0].At
}

func (f *Fleet) cleanup(ctx context.Context, dry bool, now time.Time) fleetapi.CleanupReport {
	stale := f.cfg.Cleanup.Stale
	cut := now.Add(-stale)
	rep := fleetapi.CleanupReport{At: now, DryRun: dry, Stale: Dur(stale), Items: []fleetapi.CleanupItem{}, Skipped: []fleetapi.CleanupSkip{}}
	var mu sync.Mutex
	var looked []string // places whose every item this pass saw
	add := func(where string, items []fleetapi.CleanupItem, err error) {
		mu.Lock()
		defer mu.Unlock()
		for _, it := range items {
			it.Where = where
			rep.Items = append(rep.Items, it)
		}
		if err != nil {
			rep.Skipped = append(rep.Skipped, fleetapi.CleanupSkip{Where: where, Why: err.Error()})
			return
		}
		looked = append(looked, where)
	}

	// The containers: hosts at once, a host's agents one after another (a
	// pass walks gigabytes, and a host is someone's machine too).
	var free []string
	byHost := map[string][]string{}
	for _, a := range f.cfg.AgentNames() {
		if why := f.cleanupBusy(a); why != "" {
			rep.Skipped = append(rep.Skipped, fleetapi.CleanupSkip{Where: a, Why: why})
			continue
		}
		free = append(free, a)
		h := f.cfg.Agents[a].Host
		byHost[h] = append(byHost[h], a)
	}
	var wg sync.WaitGroup
	for _, agents := range byHost {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for _, a := range agents {
				// Asked again at its turn: the agents before it took a while.
				if why := f.cleanupBusy(a); why != "" {
					mu.Lock()
					rep.Skipped = append(rep.Skipped, fleetapi.CleanupSkip{Where: a, Why: why})
					mu.Unlock()
					continue
				}
				items, err := f.cleanContainer(ctx, a, cut, dry)
				add(a, items, err)
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		if items, err, ran := f.cleanSharedCode(ctx, free, cut, dry); ran {
			add(cleanCode, items, err)
		}
	}()
	wg.Wait()
	items, err := f.cleanArchives(cut, dry, now)
	add(cleanArchive, items, err)

	sort.SliceStable(rep.Items, func(i, j int) bool {
		a, b := rep.Items[i], rep.Items[j]
		return a.Where < b.Where || a.Where == b.Where && a.Path < b.Path
	})
	sort.Slice(rep.Skipped, func(i, j int) bool { return rep.Skipped[i].Where < rep.Skipped[j].Where })
	for _, it := range rep.Items {
		if it.Action != fleetapi.CleanupKept {
			rep.Freed += it.Bytes
		}
	}
	if !dry {
		f.recordCleanup(ctx, rep, looked, now)
	}
	return rep
}

// cleanupBusy says why an agent's container is left alone this pass: a
// slot with a task, or at work (its slots share the container), or out of
// reach. "" is free.
func (f *Fleet) cleanupBusy(agent string) string {
	for _, w := range f.cfg.Workers() {
		if w.Agent != agent {
			continue
		}
		st := f.State(w.Name)
		switch {
		case st.Task != "":
			return w.Name + " has task " + st.Task
		case st.Phase == reconcile.Working:
			return w.Name + " is at work"
		case !st.UnreachableSince.IsZero():
			return "its host, " + w.Host + ", is out of reach"
		}
		open, err := f.db.Tasks(true, w.Name, 1)
		if err != nil {
			return "the ledger: " + err.Error()
		}
		if len(open) > 0 {
			return w.Name + " has task " + open[0].ID
		}
	}
	return ""
}

func (f *Fleet) cleanContainer(ctx context.Context, agent string, cut time.Time, dry bool) ([]fleetapi.CleanupItem, error) {
	ctx, cancel := context.WithTimeout(ctx, cleanupOne)
	defer cancel()
	out, err := f.cleanIn(ctx, agent, cleanup.Script, cleanup.Work(cut, dry)...)
	items, perr := cleanup.Parse(out) // what it did before it failed is still said
	return items, errors.Join(err, perr)
}

// cleanSharedCode cleans the shared code's worktrees of the agents given,
// here: <code>/.worktrees/<agent>, and a slot's <agent>-<k> or <agent>:<k>.
// Nothing else under .worktrees is the cleanup's. ran is false when none
// of them has one.
func (f *Fleet) cleanSharedCode(ctx context.Context, agents []string, cut time.Time, dry bool) (items []fleetapi.CleanupItem, err error, ran bool) {
	root := filepath.Join(f.cfg.Code, ".worktrees")
	var dirs []string
	for _, a := range agents {
		names := []string{a}
		for k := 2; k <= f.cfg.Agents[a].Slots; k++ {
			names = append(names, a+"-"+strconv.Itoa(k), a+":"+strconv.Itoa(k))
		}
		for _, n := range names {
			d := filepath.Join(root, n)
			if fi, err := os.Lstat(d); err == nil && fi.IsDir() {
				dirs = append(dirs, d)
			}
		}
	}
	if len(dirs) == 0 {
		return nil, nil, false
	}
	ctx, cancel := context.WithTimeout(ctx, cleanupOne)
	defer cancel()
	out, err := f.cleanHere(ctx, cleanup.Dirs(cut, dry, dirs...)...)
	items, perr := cleanup.Parse(out)
	return items, errors.Join(err, perr), true
}

// cleanArchives deletes the session archives made before cut, but never one
// whose task is still open: it may yet be restored for it.
func (f *Fleet) cleanArchives(cut time.Time, dry bool, now time.Time) ([]fleetapi.CleanupItem, error) {
	all, err := archive.List(f.archiveRoot(), time.Time{})
	if err != nil {
		return nil, err
	}
	var items []fleetapi.CleanupItem
	for _, m := range all {
		if !m.ArchivedAt.Before(cut) {
			continue
		}
		if m.Task != "" {
			t, err := f.db.Task(m.Task)
			if err != nil && !errors.Is(err, sql.ErrNoRows) || err == nil && t.Closed.IsZero() {
				continue
			}
		}
		dir, err := archive.Dir(f.archiveRoot(), m.ID)
		if err != nil {
			continue
		}
		it := fleetapi.CleanupItem{Path: m.ID, Bytes: dirBytes(dir), Action: fleetapi.CleanupWould,
			Why: "a session archived " + Dur(now.Sub(m.ArchivedAt)) + " ago, its task closed"}
		if m.Task == "" {
			it.Why = "a session archived " + Dur(now.Sub(m.ArchivedAt)) + " ago, on no task"
		}
		if !dry {
			it.Action = fleetapi.CleanupDeleted
			if err := os.RemoveAll(dir); err != nil {
				it.Action, it.Why = fleetapi.CleanupKept, cleanup.NotDeleted+": "+err.Error()
			}
		}
		items = append(items, it)
	}
	return items, nil
}

func dirBytes(dir string) int64 {
	var n int64
	_ = filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			if fi, err := d.Info(); err == nil {
				n += fi.Size()
			}
		}
		return nil
	})
	return n
}

// recordCleanup is a pass's event, and tells you, once each, of what it kept
// because it holds work found nowhere else.
func (f *Fleet) recordCleanup(ctx context.Context, rep fleetapi.CleanupReport, looked []string, now time.Time) {
	freedBy := map[string]int64{}
	kept := []fleetapi.CleanupItem{}
	still := map[string][]string{}
	for _, it := range rep.Items {
		switch it.Action {
		case fleetapi.CleanupDeleted:
			freedBy[it.Where] += it.Bytes
		case fleetapi.CleanupKept:
			kept = append(kept, it)
			still[it.Where] = append(still[it.Where], it.Path)
		}
	}
	b, _ := json.Marshal(map[string]any{"freed_bytes": rep.Freed, "freed_by": freedBy, "kept": kept, "skipped": rep.Skipped})
	// At the pass's time, not the write's: the next fleetd reads it as when
	// the last pass ran.
	if err := f.db.AddEvent(ledger.Event{At: now, Kind: "fleet.cleanup", Detail: string(b)}); err != nil {
		f.Log("ledger: event fleet.cleanup: %v", err)
	}
	f.touch()
	for _, p := range looked {
		if err := f.db.ForgetKept(p, still[p]); err != nil {
			f.Log("ledger: cleanup kept: %v", err)
		}
	}
	var tell []string
	for _, it := range kept {
		if strings.HasPrefix(it.Why, cleanup.NotDeleted) {
			continue
		}
		if first, err := f.db.KeptTold(it.Where, it.Path, now); err != nil {
			f.Log("ledger: cleanup kept: %v", err)
		} else if first {
			tell = append(tell, fmt.Sprintf("%s %s (%s)", it.Where, it.Path, it.Why))
		}
	}
	if len(tell) > 0 {
		f.alert(ctx, "", fmt.Sprintf("The fleet's cleanup kept %d unused %s holding work found nowhere else; push the work, or delete them once you have what you need: %s",
			len(tell), plural(len(tell), "item", "items"), strings.Join(tell, "; ")))
	}
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}
