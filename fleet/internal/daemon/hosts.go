package daemon

import (
	"bytes"
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/MelloB1989/karmax/fleet/internal/fleetapi"
	"github.com/MelloB1989/karmax/fleet/internal/ledger"
	"github.com/MelloB1989/karmax/fleet/internal/reconcile"
)

// Host samples for the watch snapshot. Once a minute per host, one exec
// into any running agent container there reads the host's load, memory and
// disk; never one per worker. Containers' cpu and memory come from the
// docker stats the tick already takes. The last hostKeep of each are kept
// in memory.

const (
	hostEvery   = time.Minute
	hostKeep    = 60
	hostTimeout = 20 * time.Second
)

// hostScript prints the host's load, memory, CPUs and the disk of $1 (the
// code directory, or /work where it is not mounted).
const hostScript = `cat /proc/loadavg; grep -E '^(MemTotal|MemAvailable):' /proc/meminfo; nproc; { df -Pk "$1" 2>/dev/null || df -Pk /work; } | tail -n 1`

// hostSample is one reading of a host.
type hostSample struct {
	At                  time.Time
	CPUs                int
	Load                [3]float64
	MemUsed, MemTotal   int64
	DiskUsed, DiskTotal int64
}

// hostSeries is what fleetd keeps of its hosts between snapshots.
type hostSeries struct {
	mu         sync.Mutex
	probed     map[string]time.Time // when each host was last asked
	hosts      map[string][]hostSample
	containers map[string][]ledger.HostSample
}

func newHostSeries() *hostSeries {
	return &hostSeries{probed: map[string]time.Time{}, hosts: map[string][]hostSample{},
		containers: map[string][]ledger.HostSample{}}
}

// keep appends to a series and keeps its last hostKeep.
func keep[T any](s []T, v T) []T {
	s = append(s, v)
	if len(s) > hostKeep {
		s = append(s[:0:0], s[len(s)-hostKeep:]...)
	}
	return s
}

// containerSamples keeps docker stats readings, by container.
func (f *Fleet) containerSamples(samples []ledger.HostSample) {
	h := f.hosts
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, s := range samples {
		h.containers[s.Container] = keep(h.containers[s.Container], s)
	}
}

// sampleHost reads a host, unless it was read within the minute: one exec,
// into the first of its agents whose container runs.
func (f *Fleet) sampleHost(ctx context.Context, host string, now time.Time) {
	h := f.hosts
	h.mu.Lock()
	if last, ok := h.probed[host]; ok && now.Sub(last) < hostEvery {
		h.mu.Unlock()
		return
	}
	h.probed[host] = now
	h.mu.Unlock()
	worker := ""
	for _, name := range f.cfg.AgentsOn(host) {
		if f.cfg.Agents[name].Sandbox {
			continue // a guest's container: what it prints is the guest's to fake
		}
		st := f.State(name)
		switch st.Phase {
		case reconcile.Down, reconcile.CodeMissing:
			continue
		}
		if st.UnreachableSince.IsZero() {
			worker = name
			break
		}
	}
	if worker == "" {
		return // nothing runs there to ask
	}
	pctx, cancel := context.WithTimeout(ctx, hostTimeout)
	defer cancel()
	out, err := f.hostExec(pctx, worker, hostScript, f.cfg.Code)
	var s hostSample
	if err == nil {
		s, err = parseHostSample(out, now)
	}
	if err != nil {
		f.tickErr(fmt.Sprintf("host %s: %v", host, err))
		return
	}
	h.mu.Lock()
	h.hosts[host] = keep(h.hosts[host], s)
	h.mu.Unlock()
}

// rshHost runs the host script in a worker's container.
func (f *Fleet) rshHost(ctx context.Context, worker, script string, args ...string) ([]byte, error) {
	var out bytes.Buffer
	res, err := f.box(worker).Rsh(ctx, append([]string{"sh", "-c", script, "sh"}, args...), nil, &out)
	if err == nil && res.Exit != 0 {
		err = fmt.Errorf("exit %d: %s", res.Exit, clip(strings.TrimSpace(res.Stderr), 200))
	}
	return out.Bytes(), err
}

// parseHostSample reads hostScript's output.
func parseHostSample(out []byte, at time.Time) (hostSample, error) {
	s := hostSample{At: at}
	var memAvail int64
	var seen int
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		switch {
		case len(f) == 0:
		case len(f) == 5 && strings.Contains(f[3], "/"): // /proc/loadavg
			for i := range 3 {
				s.Load[i], _ = strconv.ParseFloat(f[i], 64)
			}
			seen++
		case f[0] == "MemTotal:" && len(f) >= 2:
			s.MemTotal = kib(f[1])
		case f[0] == "MemAvailable:" && len(f) >= 2:
			memAvail = kib(f[1])
		case len(f) == 1:
			s.CPUs, _ = strconv.Atoi(f[0])
		case len(f) >= 6: // df -Pk: filesystem, 1024-blocks, used, available, capacity, mount
			s.DiskTotal, s.DiskUsed = kib(f[1]), kib(f[2])
		}
	}
	if seen == 0 || s.MemTotal == 0 {
		return s, fmt.Errorf("unreadable host sample: %q", clip(string(out), 200))
	}
	s.MemUsed = s.MemTotal - memAvail
	return s, nil
}

func kib(s string) int64 {
	n, _ := strconv.ParseInt(s, 10, 64)
	return n << 10
}

// hostsView is every host as the snapshot shows it: its last reading and
// load history, and each of its agents' containers with theirs.
func (f *Fleet) hostsView(now time.Time) []fleetapi.Host {
	restarts, err := f.db.Restarts(now.Add(-24 * time.Hour))
	if err != nil {
		f.Log("snapshot: restarts: %v", err)
	}
	h := f.hosts
	h.mu.Lock()
	defer h.mu.Unlock()
	out := []fleetapi.Host{}
	for _, host := range f.cfg.HostNames() {
		agents := f.cfg.AgentsOn(host)
		if len(agents) == 0 {
			continue
		}
		v := fleetapi.Host{Name: host, Load1: []fleetapi.Point{}, Containers: []fleetapi.Container{}}
		for _, name := range agents {
			st := f.State(name)
			if st.UnreachableSince.IsZero() {
				v.Reachable = true
			}
			c := fleetapi.Container{Name: f.cfg.Agents[name].Container, Agent: name, Restarts: restarts[name],
				Running: st.UnreachableSince.IsZero() && st.Phase != reconcile.Down && st.Phase != reconcile.CodeMissing,
				CPUHist: []fleetapi.Point{}, MemHist: []fleetapi.Point{}}
			cs := h.containers[c.Name]
			if k := len(cs); k > 0 {
				c.CPU, c.Mem, c.MemLimit, c.At = cs[k-1].CPU, cs[k-1].MemBytes, cs[k-1].MemLimit, cs[k-1].At
			}
			for _, s := range cs {
				c.CPUHist = append(c.CPUHist, fleetapi.Point{T: s.At, V: s.CPU})
				c.MemHist = append(c.MemHist, fleetapi.Point{T: s.At, V: float64(s.MemBytes)})
			}
			v.Containers = append(v.Containers, c)
		}
		hs := h.hosts[host]
		if k := len(hs); k > 0 {
			last := hs[k-1]
			v.CPUs, v.Load, v.MemUsed, v.MemTotal, v.DiskUsed, v.DiskTotal, v.At =
				last.CPUs, last.Load, last.MemUsed, last.MemTotal, last.DiskUsed, last.DiskTotal, last.At
		}
		for _, s := range hs {
			v.Load1 = append(v.Load1, fleetapi.Point{T: s.At, V: s.Load[0]})
		}
		out = append(out, v)
	}
	return out
}
