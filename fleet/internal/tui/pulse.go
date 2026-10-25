package tui

import (
	"fmt"
	"math"
	"slices"
	"strings"
	"time"

	"charm.land/lipgloss/v2"

	"github.com/MelloB1989/karmax/fleet/internal/fleetapi"
)

// The pulse is the strip under the Fleet tab's cards when they leave room:
// working workers over the last half hour, today's numbers, the 5-hour
// windows nearest to running out, and the latest events.

const (
	pulseSpan  = 30 * time.Minute // the sparkline's reach
	pulseEvery = 10 * time.Second // at most one sample this often
	pulseMin   = 6                // spare rows the strip needs: a gap, its rule and four rows
	pulseMax   = 10               // rows under the rule it grows to
)

// Samples is the working count the TUI has seen, a sample per snapshot and
// at most one per pulseEvery, oldest first: a ring that holds pulseSpan.
type Samples struct {
	ring    [pulseSpan / pulseEvery]fleetapi.Point
	head, n int
}

// Add records v at at. Within pulseEvery of the last sample it raises that
// sample instead, so a short burst still shows.
func (s *Samples) Add(at time.Time, v float64) {
	if s.n > 0 {
		last := &s.ring[(s.head+s.n-1)%len(s.ring)]
		if at.Sub(last.T) < pulseEvery {
			last.V = math.Max(last.V, v)
			return
		}
	}
	if s.n == len(s.ring) {
		s.head = (s.head + 1) % len(s.ring)
		s.n--
	}
	s.ring[(s.head+s.n)%len(s.ring)] = fleetapi.Point{T: at, V: v}
	s.n++
}

// Points is the samples, oldest first.
func (s *Samples) Points() []fleetapi.Point {
	out := make([]fleetapi.Point, 0, s.n)
	for i := range s.n {
		out = append(out, s.ring[(s.head+i)%len(s.ring)])
	}
	return out
}

// working is how many workers are working.
func working(s *fleetapi.Snapshot) int {
	n := 0
	for _, w := range s.Workers {
		if w.State == "working" {
			n++
		}
	}
	return n
}

// pulse is the strip, exactly w×h: blank rows, a rule naming its panels,
// then the panels side by side, at most pulseMax rows tall.
func pulse(c *Ctx, w, h int) string {
	t := c.Theme
	k := min(h-2, pulseMax)
	type panel struct {
		title, sub string
		minW       int
		draw       func(w, h int) []string
	}
	spark := panel{"Working", "last 30m", 24, func(w, h int) []string { return workingChart(c, w, h) }}
	today := panel{"Today", "since " + c.Snap.Today.Since.Local().Format("15:04"), 20, func(w, h int) []string { return todayLines(c, h) }}
	quota := panel{"Running out", "5h windows", 34, func(w, h int) []string { return runOutLines(c, w, h) }}
	events := panel{"Events", "latest", 30, func(w, h int) []string { return tickerLines(c, w, h) }}
	// The roomiest set that fits; the events take what is left.
	var ps []panel
	for _, set := range [][]panel{{spark, today, quota, events}, {spark, today, events}, {spark, events}, {spark}} {
		need := colGap * (len(set) - 1)
		for _, p := range set {
			need += p.minW
		}
		if need <= w {
			ps = set
			break
		}
	}
	if ps == nil {
		return block("", w, h)
	}
	widths := make([]int, len(ps))
	free := w - colGap*(len(ps)-1)
	for i, p := range ps {
		widths[i] = p.minW
		free -= p.minW
	}
	// The chart grows to a fifth of the strip; the rest goes to the last panel.
	grow := min(free, max(0, min(w/5, 48)-widths[0]))
	widths[0] += grow
	widths[len(widths)-1] += free - grow

	var rule strings.Builder
	cols := make([]string, len(ps))
	for i, p := range ps {
		seg := widths[i]
		if i < len(ps)-1 {
			seg += colGap
		}
		head := Fit(t.Border.Render("─ ")+t.Title.Render(p.title)+t.Muted.Render(" · "+p.sub+" "), min(seg, 2+lipgloss.Width(p.title+p.sub)+4))
		rule.WriteString(head + t.Border.Render(strings.Repeat("─", max(0, seg-lipgloss.Width(head)))))
		cols[i] = strings.Join(p.draw(widths[i], k), "\n")
	}
	gap := strings.Repeat("\n", max(0, h-k-1))
	return block(gap+Fit(rule.String(), w)+"\n"+joinColumns(cols, widths, k), w, h)
}

// workingChart is the working count over pulseSpan in h rows, w columns
// each a slice of the span, ending now; the last row is its axis. A slice
// without a sample keeps the one before it (a snapshot comes only on a
// change); before the first sample it is empty.
func workingChart(c *Ctx, w, h int) []string {
	t := c.Theme
	rows := max(1, h-1)
	pts := c.Working.Points()
	vals := make([]float64, w)
	have := make([]bool, w)
	start := c.Now.Add(-pulseSpan)
	cur, seen, j := 0.0, false, 0
	for x := range w {
		end := start.Add(pulseSpan * time.Duration(x+1) / time.Duration(w))
		inSlice := false
		for ; j < len(pts) && !pts[j].T.After(end); j++ {
			if pts[j].T.Before(start) {
				cur, seen = pts[j].V, true
				continue
			}
			if !inSlice || pts[j].V > vals[x] {
				vals[x] = pts[j].V
			}
			cur, seen, inSlice = pts[j].V, true, true
		}
		if !inSlice && seen {
			vals[x] = cur
		}
		have[x] = seen
	}
	peak := 0.0
	for x, v := range vals {
		if have[x] {
			peak = math.Max(peak, v)
		}
	}
	// A row of headroom over the peak, so a steady fleet is not a wall.
	top := peak + 1
	amber, floor := lipgloss.NewStyle().Foreground(t.Amber), t.Border
	out := make([]string, 0, h)
	for r := range rows {
		var b strings.Builder
		base := (rows - 1 - r) * 8 // the eighths under this row
		for x, v := range vals {
			fill := int(math.Round(v / top * float64(rows*8)))
			if v > 0 {
				fill = max(fill, 1)
			}
			switch n := fill - base; {
			case !have[x]:
				b.WriteByte(' ')
			case n > 0:
				b.WriteString(amber.Render(string(sparks[min(n, 8)-1])))
			case r == rows-1:
				b.WriteString(floor.Render("▁"))
			default:
				b.WriteByte(' ')
			}
		}
		out = append(out, b.String())
	}
	now := 0
	if c.Snap != nil {
		now = working(c.Snap)
	}
	right := t.Muted.Render("now ") + lipgloss.NewStyle().Foreground(t.Amber).Bold(true).Render(fmt.Sprint(now)) +
		t.Muted.Render(fmt.Sprintf(" · peak %.0f", peak))
	if len(pts) == 0 {
		right = t.Muted.Render("filling…")
	}
	if h > 1 {
		out = append(out, spread(t.Muted.Render("−30m"), right, w))
	}
	return out
}

// todayLines is Snapshot.Today: tasks closed, spend and output tokens,
// spaced out when there is room.
func todayLines(c *Ctx, h int) []string {
	t := c.Theme
	d := c.Snap.Today
	num := func(col lipgloss.Style, v, label string) string {
		return col.Bold(true).Render(fmt.Sprintf("%7s", v)) + " " + t.Muted.Render(label)
	}
	lines := []string{
		num(lipgloss.NewStyle().Foreground(t.Green), fmt.Sprint(d.TasksClosed), "tasks closed"),
		num(lipgloss.NewStyle().Foreground(t.Amber), fmt.Sprintf("$%.2f", d.SpendUSD), "spent"),
		num(lipgloss.NewStyle().Foreground(t.Cyan), Count(d.OutputTokens), "tokens out"),
	}
	if h >= 2*len(lines)-1 {
		lines = []string{lines[0], "", lines[1], "", lines[2]}
	}
	return lines
}

// runOutLines is the 5-hour windows that run out before they reset,
// soonest first, at most three: two rows each when there is room.
func runOutLines(c *Ctx, w, h int) []string {
	t := c.Theme
	var acc []fleetapi.Account
	for _, a := range c.Snap.Quota {
		if !a.RunOut.IsZero() {
			acc = append(acc, a)
		}
	}
	if len(acc) == 0 {
		return []string{paint(t.Green, "✓ ") + t.Muted.Render("no 5h window"), t.Muted.Render("  runs out before it resets")}
	}
	slices.SortStableFunc(acc, func(a, b fleetapi.Account) int { return a.RunOut.Compare(b.RunOut) })
	acc = acc[:min(3, len(acc), h)]
	two := h >= 2*len(acc)
	var out []string
	for _, a := range acc {
		left := a.RunOut.Sub(c.Now)
		col := quotaColor(t, a.FiveHour)
		when := "out " + a.RunOut.Local().Format("15:04")
		switch {
		case left <= 0:
			col, when = t.Alert, "out now"
		case left < time.Hour:
			col = t.Alert
		}
		pct := paint(quotaColor(t, a.FiveHour), fmt.Sprintf("%.0f%%", a.FiveHour))
		out = append(out, spread(lipgloss.NewStyle().Foreground(t.Text).Render(a.Name), pct+"  "+paint(col, when), w))
		if two {
			tail := fmt.Sprintf(" +%.0f%%/h", a.BurnPerHour)
			if left > 0 {
				tail = " in " + Age(left) + " ·" + tail
			}
			const tailW = 19 // " in 4h59m · +99%/h"
			out = append(out, "  "+Bar(t, a.FiveHour, max(4, w-2-tailW))+t.Muted.Render(fmt.Sprintf("%-*s", tailW, tail)))
		}
	}
	return out
}

// tickerLines is the latest three events, colored by kind as the Events
// tab: a line each, or a heading and its text when there is room.
func tickerLines(c *Ctx, w, h int) []string {
	t := c.Theme
	evs := c.Snap.Events[:min(3, len(c.Snap.Events))]
	if len(evs) == 0 {
		return []string{t.Muted.Render("no events yet")}
	}
	text := lipgloss.NewStyle().Foreground(t.Text)
	var out []string
	for _, e := range evs {
		at := t.Muted.Render(e.At.Local().Format("15:04") + " ")
		kind := paint(kindColor(t, e.Kind), strings.TrimPrefix(e.Kind, "fleet."))
		if h >= 2*len(evs) {
			out = append(out, Fit(at+kind+t.Muted.Render(" · "+e.Agent), w), Fit("      "+text.Render(eventText(e)), w))
			continue
		}
		out = append(out, Fit(at+kind+" "+t.Muted.Render(e.Agent)+" "+text.Render(eventText(e)), w))
	}
	return out
}
