package tui

import (
	"context"
	"encoding/json"
	"fmt"
	"image/color"
	"slices"
	"strings"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/MelloB1989/karmax/fleet/internal/fleetapi"
)

// fleetTab is the console's overview in the terminal: a card per agent, a
// line per slot, the busiest slot's phase down the card's left edge.
type fleetTab struct {
	detail  bool
	confirm *confirm
	picker  *modelPicker
	admin   bool
	off     int // the first line shown when the cards scroll
}

func newFleet() Tab { return &fleetTab{} }

func (f *fleetTab) Title() string            { return "Fleet" }
func (f *fleetTab) Visible(fleetapi.Me) bool { return true }
func (f *fleetTab) Capturing() bool          { return f.confirm != nil || f.picker != nil }

func (f *fleetTab) Help() []key.Binding {
	switch {
	case f.picker != nil:
		return []key.Binding{keyIs("←→", "choose"), keyIs("⏎", "set"), keyIs("esc", "drop it")}
	case f.confirm != nil:
		return []key.Binding{keyIs("y", "yes"), keyIs("n", "no")}
	}
	m := keyIs("m", "model")
	m.SetEnabled(f.admin)
	return []key.Binding{
		key.NewBinding(key.WithKeys("up", "down"), key.WithHelp("↑↓", "select")),
		key.NewBinding(key.WithKeys("enter"), key.WithHelp("⏎", "detail")),
		keyIs("s", "ssh"), keyIs("t", "type"), m, keyIs("d", "done"), keyIs("c", "cancel"),
	}
}

// group is one agent and its slots, in roster order.
type group struct {
	name  string
	slots []fleetapi.Worker
}

func groups(s *fleetapi.Snapshot) []group {
	var out []group
	if s == nil {
		return nil
	}
	for _, w := range s.Workers {
		name := w.SlotOf
		if name == "" {
			name = w.Agent
		}
		if n := len(out); n > 0 && out[n-1].name == name {
			out[n-1].slots = append(out[n-1].slots, w)
			continue
		}
		out = append(out, group{name: name, slots: []fleetapi.Worker{w}})
	}
	return out
}

// selected is the selected worker, defaulting to the first.
func (f *fleetTab) selected(c *Ctx) (fleetapi.Worker, bool) {
	if c.Snap == nil || len(c.Snap.Workers) == 0 {
		return fleetapi.Worker{}, false
	}
	for _, w := range c.Snap.Workers {
		if w.Agent == c.Selected {
			return w, true
		}
	}
	c.Selected = c.Snap.Workers[0].Agent
	return c.Snap.Workers[0], true
}

func (f *fleetTab) Update(msg tea.Msg, c *Ctx) tea.Cmd {
	f.admin = c.Snap != nil && c.Snap.Me.Role == fleetapi.RoleAdmin
	m, ok := msg.(tea.KeyPressMsg)
	if !ok {
		return nil
	}
	k := m.String()
	if f.confirm != nil {
		cf := f.confirm
		f.confirm = nil
		return cf.update(k)
	}
	if f.picker != nil {
		cmd, done := f.picker.update(k, c)
		if done {
			f.picker = nil
		}
		return cmd
	}
	w, ok := f.selected(c)
	if !ok {
		return nil
	}
	switch k {
	case "up", "k", "down", "j":
		ws := c.Snap.Workers
		i := slices.IndexFunc(ws, func(x fleetapi.Worker) bool { return x.Agent == w.Agent })
		if k == "up" || k == "k" {
			i = max(0, i-1)
		} else {
			i = min(len(ws)-1, i+1)
		}
		c.Selected = ws[i].Agent
	case "home", "g":
		c.Selected = c.Snap.Workers[0].Agent
	case "end", "G":
		c.Selected = c.Snap.Workers[len(c.Snap.Workers)-1].Agent
	case "enter":
		f.detail = !f.detail
	case "esc":
		f.detail = false
	case "s":
		return c.Attach(w.Agent, fleetapi.TermWatch)
	case "t":
		return c.Attach(w.Agent, fleetapi.TermType)
	case "m":
		if !f.admin {
			return toast("setting a model is for admins", true)
		}
		if len(w.Models) == 0 {
			return toast(w.Agent+" lists no models", true)
		}
		f.picker = &modelPicker{worker: w.Agent, models: w.Models, i: max(0, slices.Index(w.Models, w.Model))}
	case "d", "c":
		if w.Task == "" {
			return toast(w.Agent+" has no task", true)
		}
		f.confirm = taskConfirm(c, k, w.Task, w.Agent)
	}
	return nil
}

// taskConfirm asks before closing a task as done (d) or cancelling it (c).
func taskConfirm(c *Ctx, k, task, agent string) *confirm {
	cl := c.Client
	if k == "d" {
		return &confirm{ask: fmt.Sprintf("Close %s on %s as done?", task, agent), act: call("closed "+task+" as done", func(ctx context.Context) (string, error) {
			return "", cl.Done(ctx, task)
		})}
	}
	return &confirm{ask: fmt.Sprintf("Cancel %s on %s?", task, agent), act: call("cancelled "+task, func(ctx context.Context) (string, error) {
		return "", cl.Cancel(ctx, task, "cancelled from the TUI")
	})}
}

// modelPicker chooses a model for a worker: ←→ choose, ⏎ set.
type modelPicker struct {
	worker string
	models []string
	i      int
}

func (p *modelPicker) update(k string, c *Ctx) (tea.Cmd, bool) {
	switch k {
	case "left", "h", "shift+tab":
		p.i = (p.i + len(p.models) - 1) % len(p.models)
	case "right", "l", "tab":
		p.i = (p.i + 1) % len(p.models)
	case "enter":
		cl, w, m := c.Client, p.worker, p.models[p.i]
		return call(w+" now runs "+m, func(ctx context.Context) (string, error) { return "", cl.SetModel(ctx, w, m) }), true
	case "esc", "q":
		return nil, true
	}
	return nil, false
}

func (p *modelPicker) view(t *Theme, w int) string {
	parts := []string{lipgloss.NewStyle().Foreground(t.Amber).Bold(true).Render("▌ Model for " + p.worker + "  ")}
	for i, m := range p.models {
		if i == p.i {
			parts = append(parts, lipgloss.NewStyle().Foreground(t.Ground).Background(t.Amber).Bold(true).Render(" "+m+" "))
		} else {
			parts = append(parts, lipgloss.NewStyle().Foreground(t.Text).Render(" "+m+" "))
		}
	}
	parts = append(parts, t.Muted.Render("   ←→ choose · ⏎ set · esc"))
	return Fit(strings.Join(parts, ""), w)
}

// Card sizes.
const (
	cardNormalW = 64  // the narrowest card with bars
	cardNormalX = 110 // wider than this, cards with bars take another column
	cardDenseW  = 46  // the narrowest dense card
	cardDenseX  = 70  // wider than this, dense cards take another column
	colGap      = 2
	paneW       = 46 // the detail pane beside the cards
)

func (f *fleetTab) View(c *Ctx, w, h int) string {
	if c.Snap == nil || w <= 0 || h <= 0 {
		return ""
	}
	t := c.Theme
	var foot string
	switch {
	case f.confirm != nil:
		foot = f.confirm.view(t, w)
	case f.picker != nil:
		foot = f.picker.view(t, w)
	}
	if foot != "" {
		h--
	}
	if h <= 0 {
		return foot
	}
	var body string
	sel, ok := f.selected(c)
	switch {
	case len(c.Snap.Workers) == 0:
		body = center(t.Muted.Render("no agents to show")+"\n"+t.Muted.Render(emptyWhy(c.Snap.Me)), w, h)
	case f.detail && ok && w >= paneW+cardDenseW+colGap:
		cw := w - paneW - colGap
		body = joinColumns([]string{f.cards(c, cw, h), detailPane(c, sel, paneW, h, true)}, []int{cw, paneW}, h)
	case f.detail && ok:
		body = detailPane(c, sel, w, h, false)
	default:
		body = f.cards(c, w, h)
	}
	if foot != "" {
		return block(body, w, h) + "\n" + foot
	}
	return body
}

func emptyWhy(me fleetapi.Me) string {
	if me.Role == fleetapi.RoleGuest {
		return "the operator has lent this device no agents yet"
	}
	return "fleet.yaml has no agents"
}

// cards lays out every agent's card in w×h: with bars when they fit, then
// dense, then dense in one scrolling column.
func (f *fleetTab) cards(c *Ctx, w, h int) string {
	gs := groups(c.Snap)
	boxes := containers(c.Snap)
	// Roomiest first: bars and a gap between cards, then no bars, then no
	// gap either (dense).
	for _, mode := range []struct{ bars, gap bool }{{true, true}, {false, true}, {false, false}} {
		minW, maxW := cardNormalW, cardNormalX
		if !mode.bars {
			minW, maxW = cardDenseW, cardDenseX
		}
		minCols := max(1, (w+maxW-1)/maxW)
		for cols := minCols; cols <= max(minCols, (w+colGap)/(minW+colGap)); cols++ {
			cw := (w - colGap*(cols-1)) / cols
			if cw < minW && cols > 1 {
				break
			}
			var blocks [][]string
			for _, g := range gs {
				blocks = append(blocks, card(c, g, boxes, cw, mode.bars, mode.gap))
			}
			if colsOut, ok := pack(blocks, cols, h); ok {
				f.off = 0
				widths := make([]int, cols)
				for i := range widths {
					widths[i] = cw
				}
				return joinColumns(colsOut, widths, h)
			}
		}
	}
	// One scrolling column, dense, the selection kept in view.
	var lines []string
	selAt := 0
	for _, g := range gs {
		for i, l := range card(c, g, boxes, w, false, false) {
			if i > 0 && g.slots[i-1].Agent == c.Selected {
				selAt = len(lines)
			}
			lines = append(lines, l)
		}
	}
	f.off = scrollTo(f.off, selAt, len(lines), h)
	end := min(len(lines), f.off+h)
	shown := slices.Clone(lines[f.off:end])
	t := c.Theme
	if f.off > 0 {
		shown[0] = t.Muted.Render(fmt.Sprintf("  ↑ %d more", f.off))
	}
	if end < len(lines) {
		shown[len(shown)-1] = t.Muted.Render(fmt.Sprintf("  ↓ %d more", len(lines)-end))
	}
	return block(strings.Join(shown, "\n"), w, h)
}

// pack fills cols columns of at most h lines with the blocks in order,
// as evenly as it can; ok is false when they do not fit. A block's trailing
// blank line may fall off a column's foot.
func pack(blocks [][]string, cols, h int) ([]string, bool) {
	total := 0
	for _, b := range blocks {
		total += len(b)
	}
	for target := (total + cols - 1) / cols; target <= h+1; target++ {
		out := make([][]string, 0, cols)
		var col []string
		for _, b := range blocks {
			if len(col) > 0 && len(col)+len(b) > target {
				out = append(out, col)
				col = nil
			}
			col = append(col, b...)
		}
		out = append(out, col)
		if len(out) > cols {
			continue
		}
		fits := true
		res := make([]string, cols)
		for i, col := range out {
			for len(col) > 0 && len(col) > h && ansiBlank(col[len(col)-1]) {
				col = col[:len(col)-1]
			}
			if len(col) > h {
				fits = false
			}
			res[i] = strings.Join(col, "\n")
		}
		if fits {
			return res, true
		}
	}
	return nil, false
}

func ansiBlank(s string) bool { return strings.TrimSpace(s) == "" }

// joinColumns sets blocks side by side, each cut to its width, colGap apart.
func joinColumns(cols []string, widths []int, h int) string {
	split := make([][]string, len(cols))
	for i, c := range cols {
		split[i] = strings.Split(block(c, widths[i], h), "\n")
	}
	gap := strings.Repeat(" ", colGap)
	out := make([]string, h)
	for y := range out {
		parts := make([]string, len(cols))
		for i := range cols {
			parts[i] = split[i][y]
		}
		out[y] = strings.Join(parts, gap)
	}
	return strings.Join(out, "\n")
}

// containers is each container's resources, by name.
func containers(s *fleetapi.Snapshot) map[string]fleetapi.Container {
	out := map[string]fleetapi.Container{}
	for _, h := range s.Hosts {
		for _, c := range h.Containers {
			out[c.Name] = c
		}
	}
	return out
}

// lead is the slot that speaks for the card: the busiest, as the console's
// leadPhase.
func lead(slots []fleetapi.Worker) fleetapi.Worker {
	rank := map[string]int{"working": 0, "waiting": 1, "cooling": 2, "standby": 3, "disabled": 4, "down": 5}
	r := func(w fleetapi.Worker) int {
		if n, ok := rank[w.State]; ok {
			return n
		}
		return len(rank)
	}
	best := slots[0]
	for _, w := range slots[1:] {
		if r(w) < r(best) {
			best = w
		}
	}
	return best
}

// card is one agent's lines, w wide: a header, a line per slot (with the
// 5-hour bar when bars), and a blank line after when gap. Without bars the
// account's windows are in the header.
func card(c *Ctx, g group, boxes map[string]fleetapi.Container, w int, bars, gap bool) []string {
	t := c.Theme
	ld := lead(g.slots)
	edgeColor, _ := t.Phase(ld.State, ld.Alerts)
	edge := paint(edgeColor, "┃")
	iw := w - 2

	head := t.Strong.Render(g.name) + t.Muted.Render(" · "+ld.Host)
	if ld.Sandbox {
		head += " " + lipgloss.NewStyle().Foreground(t.Violet).Render("sandbox")
	}
	if alerts := len(ld.Alerts); alerts > 0 && ld.State != "down" {
		head += " " + t.Danger.Render("▲ "+strings.Join(ld.Alerts, ", "))
	}
	var right []string
	if !bars && ld.State != "down" {
		right = append(right, t.Muted.Render("5h ")+paint(quotaColor(t, ld.FiveHour), fmt.Sprintf("%.0f%%", ld.FiveHour))+
			t.Muted.Render(" 7d ")+paint(quotaColor(t, ld.SevenDay), fmt.Sprintf("%.0f%%", ld.SevenDay)))
	}
	if box, ok := boxes[ld.Container]; ok && box.Running {
		r := fmt.Sprintf("cpu %.0f%% · %s", box.CPU, Bytes(box.Mem))
		if box.MemLimit > 0 && iw >= 60 {
			r += " / " + Bytes(box.MemLimit)
		}
		right = append(right, t.Muted.Render(r))
	}
	// The header's right side gives way to the name: drop from the end.
	for len(right) > 0 && lipgloss.Width(head)+lipgloss.Width(strings.Join(right, t.Muted.Render(" · ")))+2 > iw {
		right = right[:len(right)-1]
	}
	lines := []string{edge + " " + spread(head, strings.Join(right, t.Muted.Render(" · ")), iw)}
	for _, s := range g.slots {
		lines = append(lines, edge+slotLine(c, s, iw+1, !bars))
	}
	if gap {
		lines = append(lines, "")
	}
	return lines
}

// slotLine is one worker: selection, sprite, slot, phase, what it does,
// model, context, the 5-hour bar (not dense) and its tool.
func slotLine(c *Ctx, s fleetapi.Worker, w int, dense bool) string {
	t := c.Theme
	pc, label := t.Phase(s.State, s.Alerts)
	mark, name := " ", lipgloss.NewStyle().Foreground(t.Text)
	if s.Agent == c.Selected {
		mark, name = lipgloss.NewStyle().Foreground(t.Amber).Bold(true).Render("▸"), name.Foreground(t.Amber).Bold(true)
	}
	slot := fmt.Sprintf("S%d", max(1, s.Slot))
	left := mark + " " + Sprite(t, s.Activity.Kind, c.Frame) + " " + name.Render(Fit(slot, 3)) + paint(pc, Fit(label, 9))

	// The right-hand columns, in the order drawn; when the doing column
	// would be narrower than minDoing, the least useful goes first.
	type col struct {
		s    string
		keep int // higher stays longer
	}
	tool := Fit("", 10)
	if s.Activity.Tool != "" && s.State == "working" {
		tool = paint(t.Activity(s.Activity.Kind), Fit(s.Activity.Tool+" "+Since(c.Now, s.Activity.Since), 10))
	}
	cols := []col{{t.Muted.Render(Fit(s.Model, 7)), 1}, {t.Muted.Render(fmt.Sprintf("ctx %3.0f%%", s.Context)), 2}}
	if !dense {
		cols = append(cols, col{t.Muted.Render("5h ") + Bar(t, s.FiveHour, 8) + lipgloss.NewStyle().Foreground(t.Text).Render(fmt.Sprintf("%4.0f%%", s.FiveHour)), 3})
	}
	cols = append(cols, col{tool, 4})
	const minDoing = 16
	doingW := w - lipgloss.Width(left) - 1
	for _, cl := range cols {
		doingW -= lipgloss.Width(cl.s) + 1
	}
	for len(cols) > 0 && doingW < minDoing {
		drop := 0
		for i, cl := range cols {
			if cl.keep < cols[drop].keep {
				drop = i
			}
		}
		doingW += lipgloss.Width(cols[drop].s) + 1
		cols = slices.Delete(cols, drop, drop+1)
	}
	line := left + " " + Fit(doing(c, s), max(0, doingW))
	for _, cl := range cols {
		line += " " + cl.s
	}
	return Fit(line, w)
}

// doing is what a worker is about, in a few words: its task, else why it
// is not working.
func doing(c *Ctx, s fleetapi.Worker) string {
	t := c.Theme
	switch {
	case s.Task != "":
		return lipgloss.NewStyle().Foreground(t.Text).Render(taskSlug(s.Task))
	case s.State == "cooling" && !s.Cooling.IsZero():
		return paint(t.Frost, "cools till "+s.Cooling.Format("15:04"))
	case s.Reason != "":
		return t.Muted.Render(s.Reason)
	case s.Unreachable:
		return paint(t.Alert, "unreachable")
	case s.Idle != "":
		return t.Muted.Render("idle " + s.Idle)
	}
	return t.Muted.Render("idle")
}

// detailPane is everything about one worker, w×h; with a rule down its
// left when it sits beside the cards.
func detailPane(c *Ctx, s fleetapi.Worker, w, h int, rule bool) string {
	t := c.Theme
	pc, label := t.Phase(s.State, s.Alerts)
	kv := func(k, v string) string { return t.Muted.Render(Fit(k, 9)) + v }
	text := lipgloss.NewStyle().Foreground(t.Text)
	or := func(s string) string {
		if s == "" {
			return t.Muted.Render("—")
		}
		return text.Render(s)
	}
	resets := func(at string, d string) string {
		if at == "" {
			return ""
		}
		return t.Muted.Render("  resets " + at + " (" + d + ")")
	}
	reset5, reset7 := "", ""
	if !s.FiveResets.IsZero() {
		reset5 = resets(s.FiveResets.Format("15:04"), Age(s.FiveResets.Sub(c.Now)))
	}
	if !s.SevenResets.IsZero() {
		reset7 = resets(s.SevenResets.Format("Mon 15:04"), Age(s.SevenResets.Sub(c.Now)))
	}
	bw := max(4, min(16, w-24))
	act := string(s.Activity.Kind)
	if s.Activity.Tool != "" {
		act += " · " + s.Activity.Tool
	}
	if !s.Activity.Since.IsZero() {
		act += " · " + Since(c.Now, s.Activity.Since)
	}
	alerts := t.Muted.Render("none")
	if len(s.Alerts) > 0 {
		alerts = t.Danger.Render(strings.Join(s.Alerts, ", "))
	}
	lines := []string{
		Sprite(t, s.Activity.Kind, c.Frame) + " " + t.Strong.Render(s.Agent) + "  " + paint(pc, label),
		"",
		kv("task", or(s.Task)),
		kv("doing", text.Render(act)),
		kv("model", or(s.Model)+t.Muted.Render("  of "+strings.Join(s.Models, ", "))),
		kv("host", or(s.Host)+t.Muted.Render(" · "+s.Container)),
		kv("account", or(s.Account)),
		kv("session", or(shortID(s.Session))),
		kv("ctx", Bar(t, s.Context, bw)+text.Render(fmt.Sprintf(" %.0f%%", s.Context))),
		kv("5h", Bar(t, s.FiveHour, bw)+text.Render(fmt.Sprintf(" %.0f%%", s.FiveHour))),
		kv("", reset5),
		kv("7d", Bar(t, s.SevenDay, bw)+text.Render(fmt.Sprintf(" %.0f%%", s.SevenDay))),
		kv("", reset7),
		kv("alerts", alerts),
	}
	if s.Reason != "" {
		lines = append(lines, kv("why", text.Render(s.Reason)))
	}
	if s.Idle != "" && s.State == "standby" {
		lines = append(lines, kv("idle", text.Render(s.Idle)))
	}
	lines = append(lines, "", t.Title.Render("Recent"))
	n := 0
	for _, e := range c.Snap.Events {
		if n == 5 {
			break
		}
		if e.Agent != s.Agent && (s.Task == "" || eventTask(e) != s.Task) {
			continue
		}
		lines = append(lines, t.Muted.Render(e.At.Format("15:04")+" ")+paint(kindColor(t, e.Kind), e.Kind)+" "+text.Render(eventText(e)))
		n++
	}
	if n == 0 {
		lines = append(lines, t.Muted.Render("nothing in the last 200 events"))
	}
	if !rule {
		return block(strings.Join(lines, "\n"), w, h)
	}
	edge := lipgloss.NewStyle().Foreground(t.Line).Render("│") + " "
	out := strings.Split(block(strings.Join(lines, "\n"), max(1, w-2), h), "\n")
	for i := range out {
		out[i] = edge + out[i]
	}
	return strings.Join(out, "\n")
}

func shortID(s string) string {
	if len(s) > 8 {
		return s[:8]
	}
	return s
}

// eventTask is the task an event's detail names, if any.
func eventTask(e fleetapi.Event) string {
	var d struct {
		Task string `json:"task"`
	}
	_ = json.Unmarshal(e.Detail, &d)
	return d.Task
}

// taskSlug is a task id without its date: tui-frame-1a2b for
// 20261025-tui-frame-1a2b. The full id is in the detail pane.
func taskSlug(id string) string {
	if len(id) > 9 && id[8] == '-' && strings.Trim(id[:8], "0123456789") == "" {
		return id[9:]
	}
	return id
}

// quotaColor is a window's use: plain while low, amber from 70 %, red from 90 %.
func quotaColor(t *Theme, pct float64) color.Color {
	switch {
	case pct >= 90:
		return t.Alert
	case pct >= 70:
		return t.Amber
	}
	return t.Text
}
