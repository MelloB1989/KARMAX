package console

import (
	"context"
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
	return &dockerSrc{tr: newTracker(0, true), samples: map[string]Container{}}
}

// dockerFreshness: a container missing from the frames for this long is not
// running (docker stats lists running containers only).
const dockerFreshness = 5 * time.Second

func (d *dockerSrc) run(ctx context.Context, sp spawner, prefix []string, now func() time.Time, changed func()) {
	argv := append(append([]string{}, prefix...), "stats", "--format", "{{json .}}")
	streamLines(ctx, sp, argv, d.tr, now, func() { d.tr.success(now()); changed() }, func(line []byte) {
		at := now()
		got := outbound.ParseDockerStats(line, at)
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
