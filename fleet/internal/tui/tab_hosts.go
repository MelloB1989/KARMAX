package tui

import (
	"fmt"
	"strings"
	"time"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/MelloB1989/karmax/fleet/internal/fleetapi"
)

// staleAfter is when a worker's status row is stale: three ticks at the
// default tick (fleet.yaml tick: 30s).
const staleAfter = 3 * 30 * time.Second

// hosts is the Hosts tab (not for guests): per host its load, memory and
// disk, then per container its cpu and memory, restarts, and the workers
// that are stale or unreachable; fleetd's own health on top.
type hosts struct{ off int }

func newHosts() Tab { return &hosts{} }

func (o *hosts) Title() string               { return "Hosts" }
func (o *hosts) Visible(me fleetapi.Me) bool { return me.Role == fleetapi.RoleAdmin }
func (o *hosts) Capturing() bool             { return false }
func (o *hosts) Help() []key.Binding         { return []key.Binding{quotaScroll} }

func (o *hosts) Update(msg tea.Msg, c *Ctx) tea.Cmd {
	switch keyOf(msg) {
	case "up", "k":
		o.off = max(0, o.off-1)
	case "down", "j":
		o.off++
	}
	return nil
}

// staleWorkers is the workers in a container whose status is older than
// staleAfter (or never came), and those fleetd cannot reach.
func staleWorkers(s *fleetapi.Snapshot, host, container string, now time.Time) (stale, unreachable []string) {
	for _, w := range s.Workers {
		if w.Host != host || w.Container != container {
			continue
		}
		if w.Unreachable {
			unreachable = append(unreachable, w.Agent)
		}
		if w.RecordedAt.IsZero() || now.Sub(w.RecordedAt) > staleAfter {
			stale = append(stale, w.Agent+" "+Since(now, w.RecordedAt))
		}
	}
	return stale, unreachable
}

func (o *hosts) View(c *Ctx, w, h int) string {
	t := c.Theme
	if c.Snap == nil {
		return centerNote(t, "waiting for fleetd…", w, h)
	}
	now := c.Now
	sparkW := 12
	if w < 80 {
		sparkW = 0 // under 80 columns the sparklines go
	}
	amber := lipgloss.NewStyle().Foreground(t.Amber)
	green := lipgloss.NewStyle().Foreground(t.Green)

	fd := c.Snap.Fleetd
	line := t.Title.Render("fleetd") + " " + t.Muted.Render(fd.Version) +
		"  up " + Since(now, fd.Started) +
		"  tick " + fmt.Sprintf("%dms", fd.TickMS) + t.Muted.Render(" ("+Since(now, fd.TickAt)+" ago)")
	if n := len(fd.TickErrors); n > 0 {
		line += "  " + t.Danger.Render(fmt.Sprintf("%d tick errors: %s", n, fd.TickErrors[0]))
	} else {
		line += "  " + green.Render("no tick errors")
	}
	out := []string{line}

	for _, hs := range c.Snap.Hosts {
		out = append(out, "")
		head := t.Strong.Render(hs.Name) + " "
		if !hs.Reachable {
			out = append(out, head+t.Danger.Render("✕ unreachable")+t.Muted.Render(" since "+Since(now, hs.At)))
		} else {
			load := fmt.Sprintf("load %.1f %.1f %.1f", hs.Load[0], hs.Load[1], hs.Load[2])
			if hs.CPUs > 0 && hs.Load[0] >= float64(hs.CPUs) {
				load = amber.Render(load)
			}
			head += green.Render("✓") + " " + load + t.Muted.Render(fmt.Sprintf(" /%d cpu", hs.CPUs))
			if sparkW > 0 {
				head += " " + Spark(t, hs.Load1, sparkW, 0, t.Cyan)
			}
			res := "mem " + usageBar(t, hs.MemUsed, hs.MemTotal, 8) + "  disk " + usageBar(t, hs.DiskUsed, hs.DiskTotal, 8)
			if w >= 100 {
				out = append(out, head+"  "+res)
			} else {
				out = append(out, head, "  "+res)
			}
		}
		for _, ct := range hs.Containers {
			out = append(out, containerLine(t, c.Snap, hs, ct, now, sparkW))
		}
	}
	if len(c.Snap.Hosts) == 0 {
		out = append(out, "", t.Muted.Render("no host samples yet"))
	}
	o.off = max(0, min(o.off, len(out)-h))
	return fillBody(out[o.off:], w, h)
}

func containerLine(t *Theme, s *fleetapi.Snapshot, hs fleetapi.Host, ct fleetapi.Container, now time.Time, sparkW int) string {
	l := "  " + Fit(ct.Name, 10) + " "
	if !ct.Running {
		l += t.Danger.Render(Fit("stopped", 9))
	} else {
		l += "cpu " + Fit(fmt.Sprintf("%.0f%%", ct.CPU), 4)
		if sparkW > 0 {
			l += " " + Spark(t, ct.CPUHist, sparkW, 100, t.Amber)
		}
		l += "  mem " + usageBar(t, ct.Mem, ct.MemLimit, 6)
	}
	if ct.Restarts > 0 {
		l += "  " + lipgloss.NewStyle().Foreground(t.Amber).Render(fmt.Sprintf("%d restarts", ct.Restarts))
	}
	stale, unreach := staleWorkers(s, hs.Name, ct.Name, now)
	if len(unreach) > 0 {
		l += "  " + t.Danger.Render("unreachable "+strings.Join(unreach, ","))
	}
	if len(stale) > 0 {
		l += "  " + lipgloss.NewStyle().Foreground(t.Frost).Render("stale "+strings.Join(stale, ", "))
	}
	return l
}

// usageBar is a bar of used of total, then the sizes: ███░░ 5.0G/7.0G.
func usageBar(t *Theme, used, total int64, w int) string {
	if total <= 0 {
		return t.Muted.Render("—")
	}
	p := float64(used) / float64(total) * 100
	return Bar(t, p, w) + " " + Bytes(used) + t.Muted.Render("/"+Bytes(total))
}
