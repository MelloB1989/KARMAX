package orgops

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

// A probe is the office's own eyes on production: it does not wait for a
// monitoring tool to page it. Each probe keeps a short window of results and
// flips between healthy and failing with hysteresis, so one slow request does
// not page anybody and one good request does not close a real incident.

type sample struct {
	ok      bool
	status  int
	latency time.Duration
	err     string
	at      time.Time
}

type probeState struct {
	cfg     ProbeConfig
	name    string
	window  []sample
	failing bool
	since   time.Time
}

type probeSet struct {
	mu     sync.Mutex
	states map[string]*probeState
}

func newProbeSet() *probeSet { return &probeSet{states: map[string]*probeState{}} }

// Transition is a probe changing state.
type Transition struct {
	Probe   ProbeConfig
	Firing  bool
	Title   string
	Detail  string
	Since   time.Time
	Samples string
}

func (p ProbeConfig) windowSize() int {
	if p.Window > 0 {
		return p.Window
	}
	return 10
}

func (p ProbeConfig) maxErrorRate() float64 {
	if p.MaxErrorRate > 0 {
		return p.MaxErrorRate
	}
	return 0.5
}

func (p ProbeConfig) maxLatency() time.Duration {
	d, _ := time.ParseDuration(p.MaxLatency)
	return d
}

// RunProbes checks every configured probe once and returns the transitions.
func (s *Service) RunProbes(ctx context.Context) []Transition {
	var wg sync.WaitGroup
	results := make([]sample, len(s.cfg.Probes))
	for i, p := range s.cfg.Probes {
		wg.Add(1)
		go func(i int, p ProbeConfig) {
			defer wg.Done()
			results[i] = s.probeOnce(ctx, p)
		}(i, p)
	}
	wg.Wait()
	var out []Transition
	for i, p := range s.cfg.Probes {
		if t, ok := s.probes.record(p, results[i]); ok {
			out = append(out, t)
		}
	}
	return out
}

func (s *Service) probeOnce(ctx context.Context, p ProbeConfig) sample {
	method := p.Method
	if method == "" {
		method = http.MethodGet
	}
	ctx, cancel := context.WithTimeout(ctx, p.timeout())
	defer cancel()
	start := s.now()
	req, err := http.NewRequestWithContext(ctx, method, p.URL, nil)
	if err != nil {
		return sample{err: err.Error(), at: start}
	}
	for k, v := range p.Headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	lat := time.Since(start)
	if err != nil {
		return sample{err: err.Error(), latency: lat, at: start}
	}
	resp.Body.Close()
	ok := resp.StatusCode < 400
	if p.Expect != 0 {
		ok = resp.StatusCode == p.Expect
	}
	return sample{ok: ok, status: resp.StatusCode, latency: lat, at: start}
}

func (ps *probeSet) record(p ProbeConfig, smp sample) (Transition, bool) {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	st, ok := ps.states[p.Name]
	if !ok {
		st = &probeState{cfg: p, name: p.Name}
		ps.states[p.Name] = st
	}
	st.window = append(st.window, smp)
	if n := p.windowSize(); len(st.window) > n {
		st.window = st.window[len(st.window)-n:]
	}
	unhealthy, why := st.evaluate()
	minSamples := 3
	if p.windowSize() < minSamples {
		minSamples = p.windowSize()
	}
	switch {
	case !st.failing && unhealthy && len(st.window) >= minSamples:
		st.failing, st.since = true, smp.at
		return Transition{Probe: p, Firing: true, Since: smp.at, Samples: st.recent(),
			Title:  fmt.Sprintf("%s is failing", p.Name),
			Detail: why}, true
	case st.failing && !unhealthy && st.lastOK(3):
		since := st.since
		st.failing = false
		return Transition{Probe: p, Firing: false, Since: since, Samples: st.recent(),
			Title:  fmt.Sprintf("%s has recovered", p.Name),
			Detail: fmt.Sprintf("healthy again after %s", smp.at.Sub(since).Round(time.Second))}, true
	}
	return Transition{}, false
}

func (st *probeState) evaluate() (bool, string) {
	if len(st.window) == 0 {
		return false, ""
	}
	fails := 0
	lats := make([]time.Duration, 0, len(st.window))
	for _, s := range st.window {
		if !s.ok {
			fails++
		}
		lats = append(lats, s.latency)
	}
	rate := float64(fails) / float64(len(st.window))
	if rate > st.cfg.maxErrorRate() {
		last := st.window[len(st.window)-1]
		cause := last.err
		if cause == "" {
			cause = fmt.Sprintf("HTTP %d", last.status)
		}
		return true, fmt.Sprintf("%.0f%% of the last %d checks of %s failed (latest: %s)",
			rate*100, len(st.window), st.cfg.URL, cause)
	}
	if max := st.cfg.maxLatency(); max > 0 {
		sort.Slice(lats, func(i, j int) bool { return lats[i] < lats[j] })
		if med := lats[len(lats)/2]; med > max {
			return true, fmt.Sprintf("median latency of %s is %s (limit %s)", st.cfg.URL, med.Round(time.Millisecond), max)
		}
	}
	return false, ""
}

func (st *probeState) lastOK(n int) bool {
	if len(st.window) < n {
		return false
	}
	for _, s := range st.window[len(st.window)-n:] {
		if !s.ok {
			return false
		}
	}
	return true
}

func (st *probeState) recent() string {
	var b strings.Builder
	start := 0
	if len(st.window) > 6 {
		start = len(st.window) - 6
	}
	for _, s := range st.window[start:] {
		res := fmt.Sprintf("HTTP %d", s.status)
		if s.err != "" {
			res = truncate(s.err, 80)
		}
		fmt.Fprintf(&b, "%s %s %s\n", s.at.UTC().Format("15:04:05"), res, s.latency.Round(time.Millisecond))
	}
	return b.String()
}

type probeSnap struct {
	name    string
	failing bool
	total   int
	fails   int
}

func (p probeSnap) summary() string {
	state := "healthy"
	if p.failing {
		state = "FAILING"
	}
	return fmt.Sprintf("%s (%d/%d recent checks failed)", state, p.fails, p.total)
}

func (ps *probeSet) snapshot() []probeSnap {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	out := make([]probeSnap, 0, len(ps.states))
	for _, st := range ps.states {
		f := 0
		for _, s := range st.window {
			if !s.ok {
				f++
			}
		}
		out = append(out, probeSnap{name: st.name, failing: st.failing, total: len(st.window), fails: f})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out
}
