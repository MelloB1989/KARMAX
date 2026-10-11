package console

import (
	"context"
	"regexp"
	"sync"
	"time"

	"github.com/MelloB1989/karmax/fleet/internal/outbound"
)

// dockerSrc is one long-running `docker stats`, keyed by container name.
type dockerSrc struct {
	tr      *tracker
	mu      sync.Mutex
	samples map[string]Container
}

func newDockerSrc() *dockerSrc {
	return &dockerSrc{tr: newTracker(CadenceDocker, true), samples: map[string]Container{}}
}

// csi matches an ANSI control sequence: ESC [ parameters intermediates final.
// Docker 29 streams `docker stats` lines wrapped in them (ESC[H before a
// frame's first line, ESC[K after every line); Docker 28 prints them plain.
var csi = regexp.MustCompile("\x1b\\[[0-?]*[ -/]*[@-~]")

func stripANSI(b []byte) []byte { return csi.ReplaceAll(b, nil) }

// dockerFreshness: a container missing from the frames for this long is not
// running (docker stats lists running containers only).
const dockerFreshness = 5 * time.Second

func (d *dockerSrc) run(ctx context.Context, sp spawner, prefix []string, now func() time.Time, changed func()) {
	argv := append(append([]string{}, prefix...), "stats", "--format", "{{json .}}")
	streamLines(ctx, sp, argv, d.tr, now, func() { d.tr.start(now()); changed() }, func(line []byte) {
		at := now()
		got := outbound.ParseDockerStats(stripANSI(line), at)
		if len(got) == 0 {
			return
		}
		d.mu.Lock()
		for _, s := range got {
			d.samples[s.Container] = Container{Running: true, CPUPct: s.CPU, Mem: s.MemBytes,
				MemLimit: s.MemLimit, NetRx: s.NetRx, NetTx: s.NetTx, At: s.At}
		}
		d.mu.Unlock()
		d.tr.success(at)
		changed()
	})
}

// container is the last sample for name, or nil if there never was one.
func (d *dockerSrc) container(name string, now time.Time) *Container {
	d.mu.Lock()
	defer d.mu.Unlock()
	c, ok := d.samples[name]
	if !ok {
		return nil
	}
	c.Running = now.Sub(c.At) <= dockerFreshness
	return &c
}
