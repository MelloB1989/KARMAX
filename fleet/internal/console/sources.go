package console

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/MelloB1989/karmax/fleet/internal/target"
)

// Sources is every observation the console makes. Run starts them; Fill
// writes what they know into a Snapshot.
type Sources struct {
	cfg *Config
	run target.Runner
	now func() time.Time

	// Terminals reports the terminal source's health; Terms sets it.
	Terminals func() Source

	spawn    spawner
	procRoot string
	host     struct {
		tr   *tracker
		mu   sync.Mutex
		info HostInfo
	}
	git struct {
		tr    *tracker
		mu    sync.Mutex
		repos []Repo
	}
	docker *dockerSrc
	status *statuslineSrc
	events *eventsSrc
	fleetd *fleetdSrc
	fcl    *FleetdClient
}

// NewSources builds the sources for cfg. run is how host commands (git) and
// container commands run; production passes target.Exec{}.
func NewSources(cfg *Config, run target.Runner, now func() time.Time) *Sources {
	if now == nil {
		now = time.Now
	}
	s := &Sources{cfg: cfg, run: run, now: now, spawn: execSpawn, procRoot: "/proc"}
	s.host.tr = newTracker(CadenceHost, false)
	s.git.tr = newTracker(CadenceGit, false)
	s.docker = newDockerSrc()
	live, all := s.liveNames(), s.names()
	s.status = newStatuslineSrc(live)
	s.events = newEventsSrc(live)
	s.fcl = NewFleetdClient(cfg.FleetdURL, cfg.FleetdTokenFile)
	s.fleetd = newFleetdSrc(s.fcl, now, cfg.Orchestrator, all)
	return s
}

// Fleetd is the client for fleetd, shared with the proxied routes.
func (s *Sources) Fleetd() *FleetdClient { return s.fcl }

func (s *Sources) names() []string {
	var n []string
	for _, a := range s.cfg.Agents {
		n = append(n, a.Name)
	}
	sort.Strings(n)
	return n
}

func (s *Sources) liveNames() []string {
	var n []string
	for _, a := range s.cfg.Agents {
		if a.Live {
			n = append(n, a.Name)
		}
	}
	sort.Strings(n)
	return n
}

func (s *Sources) containers() map[string]target.Container {
	m := map[string]target.Container{}
	for _, a := range s.cfg.Agents {
		if a.Live {
			m[a.Name] = target.Container{Exec: a.Exec, Name: a.Container, Runner: s.run}
		}
	}
	return m
}

// Run starts every source and blocks until ctx is done. onChange is called
// after any source updates; it must be cheap (the hub debounces).
func (s *Sources) Run(ctx context.Context, onChange func()) {
	var wg sync.WaitGroup
	go1 := func(f func()) { wg.Add(1); go func() { defer wg.Done(); f() }() }

	go1(func() {
		every(ctx, CadenceHost, func(context.Context) {
			info, err := readHost(s.procRoot)
			if err != nil {
				s.host.tr.failure(err)
			} else {
				info.At = s.now()
				s.host.mu.Lock()
				s.host.info = info
				s.host.mu.Unlock()
				s.host.tr.success(info.At)
			}
			onChange()
		})
	})
	go1(func() {
		every(ctx, CadenceGit, func(ctx context.Context) {
			s.pollGit(ctx)
			onChange()
		})
	})
	if cs := s.containers(); len(cs) > 0 {
		go1(func() {
			every(ctx, CadenceStatusline, func(ctx context.Context) {
				s.status.poll(ctx, cs, s.now())
				onChange()
			})
		})
		var prefix []string
		for _, a := range s.cfg.Agents {
			if a.Live {
				prefix = a.Exec
				break
			}
		}
		go1(func() { s.docker.run(ctx, s.spawn, prefix, s.now, onChange) })
		for _, a := range s.cfg.Agents {
			if a.Live {
				go1(func() { s.events.run(ctx, s.spawn, a.Exec, a.Container, a.Name, s.now, onChange) })
			}
		}
	}
	go1(func() { s.fleetd.run(ctx, onChange) })
	wg.Wait()
}

func (s *Sources) pollGit(ctx context.Context) {
	now := s.now()
	var repos []Repo
	var firstErr error
	for _, p := range s.cfg.Repos {
		r, err := readRepo(ctx, s.run, p, now)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			// keep the last good value for this repo, with its old At
			s.git.mu.Lock()
			for _, old := range s.git.repos {
				if old.Name == filepath.Base(p) {
					repos = append(repos, old)
				}
			}
			s.git.mu.Unlock()
			continue
		}
		repos = append(repos, r)
	}
	s.git.mu.Lock()
	s.git.repos = repos
	s.git.mu.Unlock()
	if firstErr != nil {
		s.git.tr.failure(firstErr)
		return
	}
	s.git.tr.success(now)
}

// Fill writes every field the sources own into snap, each with the time it
// was observed. A source that is down keeps its last good values and says so
// in snap.Sources.
func (s *Sources) Fill(snap *Snapshot) {
	now := s.now()
	snap.At = now
	snap.Sources = map[string]Source{
		SrcHost:       s.host.tr.source(now),
		SrcGit:        s.git.tr.source(now),
		SrcDocker:     s.docker.tr.source(now),
		SrcStatusline: s.status.source(now),
		SrcEvents:     s.events.source(now),
		SrcFleetd:     s.fleetd.tr.source(now),
		SrcTerminals:  {Error: "not started"},
	}
	if s.Terminals != nil {
		snap.Sources[SrcTerminals] = s.Terminals()
	}

	s.host.mu.Lock()
	snap.Host = s.host.info
	s.host.mu.Unlock()
	if s.cfg.Host != "" {
		snap.Host.Name = s.cfg.Host
	} else if snap.Host.Name == "" {
		snap.Host.Name, _ = os.Hostname()
	}

	s.git.mu.Lock()
	snap.Git = Git{Repos: append([]Repo{}, s.git.repos...)}
	s.git.mu.Unlock()

	f := s.fleetd
	f.mu.Lock()
	rows, last := f.rows, f.lastTask
	polled := f.polledAt
	snap.Orchestrator = f.orch
	snap.Tasks = append([]Task{}, f.tasks...)
	snap.Today = f.today
	recorded := append([]Event(nil), f.events...)
	fleetdAct := f.activity
	f.mu.Unlock()
	sort.Slice(snap.Tasks, func(i, j int) bool { return snap.Tasks[i].Since.Before(snap.Tasks[j].Since) })

	snap.Events = mergeEvents(s.events.liveEvents(), recorded)

	snap.Agents = snap.Agents[:0:0]
	snap.Activity = map[string][]Bucket{}
	confs := append([]AgentConf(nil), s.cfg.Agents...)
	sort.Slice(confs, func(i, j int) bool { return confs[i].Name < confs[j].Name })
	for _, c := range confs {
		a := Agent{ID: c.Name, Host: c.Host, Live: c.Live, Alerts: []string{}, Models: []string{}}
		if r, ok := rows[c.Name]; ok {
			a.Phase, a.Task, a.IdleS, a.Session = r.State, r.Task, r.IdleS, r.Session
			if r.Alerts != nil {
				a.Alerts = r.Alerts
			}
			if r.Models != nil {
				a.Models = r.Models
			}
			a.RecordedAt = r.RecordedAt
			if a.RecordedAt.IsZero() {
				a.RecordedAt = polled // fleetd did not date it; the poll that saw it did
			}
			a.Model = r.Model
		}
		a.LastTask = last[c.Name]
		if c.Live {
			a.Term = s.cfg.Term
			if sl, ok := s.status.get(c.Name); ok {
				if sl.Model != "" {
					a.Model = sl.Model
				}
				a.ContextPct, a.FiveHourPct, a.SevenDayPct = sl.ContextPct, sl.FiveHourPct, sl.SevenDayPct
				a.FiveHourResets, a.StatuslineAt = sl.FiveHourResets, sl.At
			}
			a.CurrentTool, a.LastEventAt = s.events.tool(c.Name)
			a.Container = s.docker.container(c.Container, now)
		}
		snap.Agents = append(snap.Agents, a)
		snap.Activity[c.Name] = mergeBuckets(fleetdAct[c.Name], s.events.buckets(c.Name, now))
	}
}

// mergeBuckets takes the larger count per bucket: fleetd's record and the
// console's live count are the same events, so adding would double them;
// the live count fills the buckets fleetd has not yet recorded.
func mergeBuckets(recorded, live []Bucket) []Bucket {
	by := map[int64]Bucket{}
	for _, b := range append(append([]Bucket{}, recorded...), live...) {
		k := b.T.Unix()
		if old, ok := by[k]; !ok || b.N > old.N {
			by[k] = b
		}
	}
	out := make([]Bucket, 0, len(by))
	for _, b := range by {
		out = append(out, b)
	}
	sortBuckets(out)
	return out
}
