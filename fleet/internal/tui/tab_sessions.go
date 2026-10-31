package tui

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"image/color"
	"path"
	"slices"
	"strings"
	"time"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/MelloB1989/karmax/fleet/internal/fleetapi"
)

const (
	deskEvery   = 2 * time.Second  // how often the shown desk is polled
	deskTimeout = 20 * time.Second // one poll's bound
	// deskLost: an answer this late went to another tab (this one was
	// hidden) or nowhere; the next poll goes without it.
	deskLost     = 5 * time.Second
	deskTileW    = 52 // the grid has width ÷ deskTileW columns …
	deskCols     = 3  // … at most three
	deskTileH    = 8  // and rows of at least this many lines
	dirsWait     = 150 * time.Millisecond
	restoredNew  = 15 * time.Minute // a restore this recent is badged
	deskSuggests = 5                // directories the launch form lists at once
)

// deskTab is the Sessions tab (admins): the operator's own tmux sessions on
// fleetd's machine, the desk, as a grid of live screens, polled every 2 s
// while shown. ⏎ attaches and detaching comes back here; n launches one.
type deskTab struct {
	list    *fleetapi.DeskList   // the last answer, keeping screens a failed capture would blank
	seen    map[string]time.Time // when each session's screen arrived, by this clock
	err     string               // the last poll's failure; "" once one answers
	sel     string               // the selected session
	selI    int                  // and its place, kept when it goes
	zoom    bool
	off     int  // the first row of tiles in view
	w, h    int  // the body's size, from the last View
	lines   int  // the lines the last poll asked for
	flying  bool // a poll is out
	sentAt  time.Time
	now     bool // poll at once: r, a change, or back from a session
	away    bool // an attach is out; the app's toast says it is back
	confirm *confirm
	form    *launchForm
	attach  bool     // attach on launch
	recent  []string // directories launched from here, newest first
}

func newSessions() Tab { return &deskTab{seen: map[string]time.Time{}, attach: true} }

type (
	deskMsg struct {
		list *fleetapi.DeskList
		err  error
	}
	deskTickMsg struct{}
	deskMadeMsg struct {
		s   *fleetapi.DeskSession
		err error
	}
	deskEndedMsg struct {
		name string
		err  error
	}
	// dirsTickMsg is a pause in typing the directory: n is its edit count.
	dirsTickMsg struct{ n int }
	dirsMsg     struct {
		n    int
		dirs []string
		err  error
	}
)

func (d *deskTab) Title() string               { return "Sessions" }
func (d *deskTab) Visible(me fleetapi.Me) bool { return me.Role == fleetapi.RoleAdmin }
func (d *deskTab) Capturing() bool             { return d.form != nil || d.confirm != nil }

var deskKeys = struct{ Move, Attach, Watch, New, End, Zoom, Refresh, Quick key.Binding }{
	Move:    key.NewBinding(key.WithKeys("up", "down", "left", "right", "k", "j", "h", "l"), key.WithHelp("←↑↓→", "select")),
	Attach:  key.NewBinding(key.WithKeys("enter", "a"), key.WithHelp("⏎/a", "attach")),
	Watch:   keyIs("w", "watch"),
	New:     keyIs("n", "new"),
	End:     keyIs("x", "end"),
	Zoom:    keyIs("z", "zoom"),
	Refresh: keyIs("r", "refresh"),
	Quick:   key.NewBinding(key.WithKeys("alt+1"), key.WithHelp("alt+1–9", "attach Nth")),
}

func (d *deskTab) Help() []key.Binding {
	k := deskKeys
	switch {
	case d.form != nil:
		return []key.Binding{keyIs("⏎", "launch"), keyIs("tab", "complete"), keyIs("↑↓", "choose"), keyIs("ctrl+a", "attach on launch"), keyIs("esc", "cancel")}
	case d.confirm != nil:
		return []key.Binding{keyIs("y", "end it"), keyIs("n", "keep it")}
	case d.zoom:
		return []key.Binding{k.Move, k.Attach, k.Watch, keyIs("esc", "grid")}
	}
	return []key.Binding{k.Move, k.Attach, k.Watch, k.New, k.End, k.Zoom, k.Refresh, k.Quick}
}

// sessions is the desk in its order.
func (d *deskTab) sessions() []fleetapi.DeskSession {
	if d.list == nil {
		return nil
	}
	return d.list.Sessions
}

// host is the machine the desk is on.
func (d *deskTab) host() string {
	if d.list == nil || d.list.Host == "" {
		return "fleetd's machine"
	}
	return d.list.Host
}

// reselect keeps the selection on its session as the desk changes, or on
// its place when that session has gone.
func (d *deskTab) reselect() {
	ss := d.sessions()
	if len(ss) == 0 {
		d.sel, d.selI, d.zoom = "", 0, false
		return
	}
	if i := slices.IndexFunc(ss, func(s fleetapi.DeskSession) bool { return s.Name == d.sel }); i >= 0 {
		d.selI = i
		return
	}
	d.selI = min(max(d.selI, 0), len(ss)-1)
	d.sel = ss[d.selI].Name
}

// setList takes sessions as the desk, in their order.
func (d *deskTab) setList(host string, ss []fleetapi.DeskSession) {
	slices.SortStableFunc(ss, func(a, b fleetapi.DeskSession) int { return cmp.Compare(a.Order, b.Order) })
	d.list = &fleetapi.DeskList{Host: host, Sessions: ss}
	d.reselect()
}

func ceilDiv(a, b int) int { return (a + b - 1) / b }

// deskGrid lays n tiles out in a w×h body: columns (1–3), rows in view and
// the tile height. When every tile fits the columns are balanced, so four
// tiles are 2×2, not 3 and 1.
func deskGrid(n, w, h int) (cols, rows, tileH int) {
	cols = min(deskCols, max(1, w/deskTileW))
	fit := max(1, h/deskTileH)
	if n > 0 && n <= cols*fit {
		cols = min(cols, n)
		for cols > 1 && ceilDiv(n, cols-1) == ceilDiv(n, cols) {
			cols--
		}
		rows = ceilDiv(n, cols)
	} else {
		rows = fit
	}
	return cols, rows, max(1, h/rows)
}

// gridH is the grid's height: the body less the header and a question.
func (d *deskTab) gridH() int {
	h := d.h - 1
	if d.confirm != nil {
		h--
	}
	return max(0, h)
}

// tileLines is how many lines of screen a tile shows: the zoomed one's,
// or, before the desk is known, a lone tile's.
func (d *deskTab) tileLines() int {
	if d.w <= 0 || d.h <= 0 {
		return 40
	}
	if d.zoom {
		return max(1, d.h-2)
	}
	_, _, th := deskGrid(max(1, len(d.sessions())), d.w, d.gridH())
	return max(1, th-2)
}

func (d *deskTab) Update(msg tea.Msg, c *Ctx) tea.Cmd {
	var cmds []tea.Cmd
	switch m := msg.(type) {
	case tea.WindowSizeMsg:
		if d.w == 0 { // until the first View: about the body's size
			d.w, d.h = max(0, m.Width-4), max(0, m.Height-frameRows)
		}
	case deskMsg:
		d.flying = false
		d.take(m, c)
		cmds = append(cmds, tea.Tick(deskEvery, func(time.Time) tea.Msg { return deskTickMsg{} }))
	case deskMadeMsg:
		cmds = append(cmds, d.made(m, c))
	case deskEndedMsg:
		cmds = append(cmds, d.ended(m))
	case dirsTickMsg:
		if d.form != nil && m.n == d.form.n {
			cmds = append(cmds, d.form.lookup(c))
		}
	case dirsMsg:
		if d.form != nil {
			d.form.offered(m)
		}
	case tea.PasteMsg:
		if d.form != nil {
			cmds = append(cmds, d.form.typeInto(m))
		}
	case ToastMsg:
		if d.away { // back from a session
			d.away, d.now = false, true
		}
	case tea.KeyPressMsg:
		cmds = append(cmds, d.key(m, c))
	}
	return tea.Batch(append(cmds, d.fetch(c))...)
}

// take is a poll's answer. A failed poll keeps the screens shown, marked
// with their age (tile); so does a session whose own capture failed.
func (d *deskTab) take(m deskMsg, c *Ctx) {
	if m.err != nil || m.list == nil {
		d.err = "fleetd sent no desk"
		if m.err != nil {
			d.err = m.err.Error()
		}
		return
	}
	d.err = ""
	old := map[string]fleetapi.DeskSession{}
	for _, s := range d.sessions() {
		old[s.Name] = s
	}
	ss := slices.Clone(m.list.Sessions)
	for i, s := range ss {
		if o := old[s.Name]; s.Text == "" && s.Error != "" && o.Text != "" {
			ss[i].Text, ss[i].Cols, ss[i].At = o.Text, o.Cols, o.At
			continue
		}
		d.seen[s.Name] = c.Now
	}
	d.setList(m.list.Host, ss)
}

// fetch polls the desk when the last poll is 2 s old, one is wanted now,
// or the tiles now hold more lines than it asked for; one poll at a time.
func (d *deskTab) fetch(c *Ctx) tea.Cmd {
	if c.Client == nil {
		return nil
	}
	lines := d.tileLines()
	if d.flying && c.Now.Sub(d.sentAt) < deskLost {
		return nil // its answer polls again if it is not enough
	}
	if !d.now && lines <= d.lines && c.Now.Sub(d.sentAt) < deskEvery {
		return nil
	}
	d.lines, d.flying, d.sentAt, d.now = lines, true, c.Now, false
	cl := c.Client
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), deskTimeout)
		defer cancel()
		l, err := cl.Desk(ctx, lines)
		return deskMsg{list: l, err: err}
	}
}

func (d *deskTab) key(m tea.KeyPressMsg, c *Ctx) tea.Cmd {
	k := m.String()
	if d.confirm != nil {
		cf := d.confirm
		d.confirm = nil
		return cf.update(k)
	}
	if d.form != nil {
		return d.formKey(m, c)
	}
	switch k {
	case "n":
		d.form = newLaunchForm(c.Theme, d.recents())
		if len(d.form.recents) == 0 { // fleetd's starting points: home, the code
			return d.form.lookup(c)
		}
		return nil
	case "r":
		d.now = true
		return nil
	}
	ss := d.sessions()
	if len(ss) == 0 {
		return nil
	}
	d.reselect()
	step := 1
	if !d.zoom { // up and down move a row; zoomed, every move is one tile
		step, _, _ = deskGrid(len(ss), d.w, d.gridH())
	}
	i := d.selI
	switch k {
	case "up", "k":
		i = max(0, i-step)
	case "down", "j":
		i = min(len(ss)-1, i+step)
	case "left", "h":
		i = max(0, i-1)
	case "right", "l":
		i = min(len(ss)-1, i+1)
	case "enter", "a":
		return d.attachTo(c, ss[i].Name, fleetapi.TermType)
	case "w":
		return d.attachTo(c, ss[i].Name, fleetapi.TermWatch)
	case "z":
		d.zoom = !d.zoom
	case "esc":
		d.zoom = false
	case "x":
		d.confirm = d.endConfirm(c, ss[i])
	default:
		if n, ok := quickKey(k); ok {
			if n >= len(ss) {
				return toastCmd(fmt.Sprintf("there is no session %d", n+1), true)
			}
			d.selI, d.sel = n, ss[n].Name
			return d.attachTo(c, ss[n].Name, fleetapi.TermType)
		}
	}
	d.selI, d.sel = i, ss[i].Name
	return nil
}

// quickKey is alt+1 … alt+9's session, from 0.
func quickKey(k string) (int, bool) {
	if d, ok := strings.CutPrefix(k, "alt+"); ok && len(d) == 1 && d[0] >= '1' && d[0] <= '9' {
		return int(d[0] - '1'), true
	}
	return 0, false
}

// attachTo joins session name in this terminal; the TUI comes back when
// its terminal closes, and polls at once.
func (d *deskTab) attachTo(c *Ctx, name string, mode fleetapi.TermMode) tea.Cmd {
	if c.Attach == nil {
		return toastCmd("attaching is not available here", true)
	}
	d.away = true
	return c.Attach(fleetapi.DeskWorker(name), mode)
}

func (d *deskTab) endConfirm(c *Ctx, s fleetapi.DeskSession) *confirm {
	why := "Its Claude Code session stays on disk."
	if !isClaude(s.Command) {
		why = "What runs in it stops."
	}
	cl, name := c.Client, s.Name
	return &confirm{ask: fmt.Sprintf("End %s? %s", name, why), act: func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
		defer cancel()
		return deskEndedMsg{name: name, err: cl.DeskEnd(ctx, name)}
	}}
}

func (d *deskTab) ended(m deskEndedMsg) tea.Cmd {
	if m.err != nil {
		return toastCmd(m.err.Error(), true)
	}
	ss := slices.DeleteFunc(slices.Clone(d.sessions()), func(s fleetapi.DeskSession) bool { return s.Name == m.name })
	delete(d.seen, m.name)
	d.setList(d.host(), ss)
	d.now = true
	return toastCmd("ended "+m.name, false)
}

// made is a launch's answer: the new session is selected and shown at
// once, and attached unless the form said not to (or was dropped).
func (d *deskTab) made(m deskMadeMsg, c *Ctx) tea.Cmd {
	f := d.form
	if m.err != nil {
		if f != nil {
			f.busy, f.err = false, m.err.Error()
			return nil
		}
		return toastCmd(m.err.Error(), true)
	}
	s := *m.s
	d.form = nil
	d.recent = append([]string{s.Dir}, slices.DeleteFunc(d.recent, func(x string) bool { return x == s.Dir })...)
	ss := slices.DeleteFunc(slices.Clone(d.sessions()), func(x fleetapi.DeskSession) bool { return x.Name == s.Name })
	d.seen[s.Name] = c.Now
	d.setList(d.host(), append(ss, s))
	d.sel, d.now = s.Name, true
	d.reselect()
	if f != nil && d.attach {
		return d.attachTo(c, s.Name, fleetapi.TermType)
	}
	return toastCmd("launched "+s.Name+" · ⏎ attaches", false)
}

// recents is the directories offered before anything is typed: those
// launched from here, then the desk's, newest first.
func (d *deskTab) recents() []string {
	ss := slices.Clone(d.sessions())
	slices.SortStableFunc(ss, func(a, b fleetapi.DeskSession) int { return b.Created.Compare(a.Created) })
	all := slices.Clone(d.recent)
	for _, s := range ss {
		all = append(all, s.Dir)
	}
	var out []string
	for _, x := range all {
		if x != "" && !slices.Contains(out, x) {
			out = append(out, x)
		}
	}
	return out
}

// isClaude: the command runs Claude Code.
func isClaude(cmd string) bool {
	f := strings.Fields(cmd)
	return len(f) == 0 || f[0] == "claude"
}

// cmdLabel is a session's command in a word: claude, its model, shell, or
// the program it runs.
func cmdLabel(cmd string) string {
	f := strings.Fields(cmd)
	switch {
	case len(f) == 0:
		return "claude"
	case f[0] == "claude":
		for i, x := range f {
			if v, ok := strings.CutPrefix(x, "--model="); ok {
				return v
			}
			if x == "--model" && i+1 < len(f) {
				return f[i+1]
			}
		}
		return "claude"
	}
	return path.Base(f[0])
}

// cmdColor is a command label's color.
func cmdColor(t *Theme, label string) color.Color {
	switch label {
	case "claude":
		return t.Amber
	case "opus":
		return t.Violet
	case "sonnet":
		return t.Cyan
	case "haiku":
		return t.Frost
	case "shell":
		return t.Green
	}
	return t.Dim
}

// shortDir is a directory as its owner writes it: their home as ~.
func shortDir(dir string) string {
	if rest, ok := strings.CutPrefix(dir, "/home/"); ok {
		if _, tail, ok := strings.Cut(rest, "/"); ok {
			return "~/" + tail
		}
		return "~"
	}
	if dir == "/root" {
		return "~"
	}
	if rest, ok := strings.CutPrefix(dir, "/root/"); ok {
		return "~/" + rest
	}
	return dir
}

var deskSpinner = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

func (d *deskTab) View(c *Ctx, w, h int) string {
	d.w, d.h = w, h
	if w <= 0 || h <= 0 {
		return ""
	}
	th := c.Theme
	bh := h
	if d.confirm != nil {
		bh--
	}
	var body string
	switch ss := d.sessions(); {
	case d.list == nil && d.err != "":
		body = center(th.Danger.Render("the desk did not answer")+"\n\n"+th.Muted.Render(oneLine(d.err)), w, bh)
	case d.list == nil:
		body = centerNote(th, "reading the desk…", w, bh)
	case len(ss) == 0:
		body = d.emptyView(c, w, bh)
	case d.zoom:
		d.reselect()
		body = strings.Join(d.tile(c, d.selI, ss[d.selI], true, w, bh), "\n")
	default:
		body = d.grid(c, w, bh)
	}
	if d.form != nil {
		body = overlay(th, block(body, w, bh), d.formView(c, w, bh), w, bh)
	}
	if d.confirm == nil {
		return block(body, w, h)
	}
	if bh <= 0 {
		return d.confirm.view(th, w)
	}
	return block(body, w, bh) + "\n" + d.confirm.view(th, w)
}

// grid is the header over the tiles in view, exactly w×h.
func (d *deskTab) grid(c *Ctx, w, h int) string {
	ss := d.sessions()
	gh := max(0, h-1)
	cols, rows, tileH := deskGrid(len(ss), w, gh)
	d.reselect()
	d.off = keepInView(d.off, d.selI/cols, ceilDiv(len(ss), cols), rows)
	tileW := w / cols
	var out []string
	for r := 0; r < rows; r++ {
		row := make([]string, tileH)
		for k := 0; k < cols; k++ {
			i := (d.off+r)*cols + k
			tw := tileW
			if k == cols-1 {
				tw = w - tileW*(cols-1) // the last column takes the remainder
			}
			var t []string
			if i < len(ss) {
				t = d.tile(c, i, ss[i], i == d.selI, tw, tileH)
			} else {
				t = strings.Split(fillBody(nil, tw, tileH), "\n")
			}
			for j := range row {
				row[j] += t[j]
			}
		}
		out = append(out, row...)
	}
	shown := min(len(ss), (d.off+rows)*cols) - d.off*cols
	return fillBody(append([]string{d.header(c, w, len(ss)-shown)}, out...), w, h)
}

// header is the desk in a line: where it is, how many, how busy; and why
// it is stale, or how many tiles are out of view.
func (d *deskTab) header(c *Ctx, w, hidden int) string {
	th := c.Theme
	ss := d.sessions()
	busy, att := 0, 0
	for _, s := range ss {
		if s.Busy {
			busy++
		}
		att += s.Clients
	}
	noun := "sessions"
	if len(ss) == 1 {
		noun = "session"
	}
	left := th.Strong.Render(d.host()) + th.Muted.Render(fmt.Sprintf(" · %d %s", len(ss), noun))
	if busy > 0 {
		left += th.Muted.Render(" · ") + paint(th.Amber, fmt.Sprintf("%d working", busy))
	}
	if att > 0 {
		left += th.Muted.Render(" · ") + paint(th.Green, fmt.Sprintf("%d attached", att))
	}
	var right string
	switch {
	case d.err != "":
		right = th.Danger.Render("⚠ ") + th.Muted.Render(oneLine(d.err))
	case hidden > 0:
		right = th.Muted.Render(fmt.Sprintf("↓ %d more", hidden))
	}
	if right == "" {
		return Fit(left, w)
	}
	return spread(left, ansi.Truncate(right, max(0, w*2/3), "…"), w)
}

// tile is one session, exactly w×h: its number, state, name, directory and
// command on the top border, its screen inside, and below how it is.
func (d *deskTab) tile(c *Ctx, i int, s fleetapi.DeskSession, sel bool, w, h int) []string {
	th := c.Theme
	in := max(0, w-2)
	dot := th.Green
	switch {
	case s.Error != "" && s.Text == "":
		dot = th.Alert
	case !s.Running:
		dot = th.Grey
	case s.Busy:
		dot = th.Amber
	}
	name := th.Strong.Render(s.Name)
	if sel {
		name = th.Selected.Render(s.Name)
	}
	left := paint(dot, "●") + " " + name
	if i < 9 {
		left = th.Muted.Render(fmt.Sprint(i+1)) + " " + left
	}
	if old := d.stale(c, s); old != "" {
		left += " " + th.Danger.Render("⚠ "+old+" old")
	}
	label := cmdLabel(s.Command)
	badge := lipgloss.NewStyle().Foreground(cmdColor(th, label)).Bold(true).Render(label)
	if dir := th.Muted.Render(shortDir(s.Dir)); lipgloss.Width(left)+2+lipgloss.Width(dir)+lipgloss.Width(badge)+6 <= in {
		left += "  " + dir
	}

	var state string
	switch {
	case !s.Running:
		state = th.Muted.Render("stopped")
	case s.Busy:
		state = paint(th.Amber, deskSpinner[c.Frame%len(deskSpinner)]+" working")
	case isClaude(s.Command):
		state = th.Muted.Render("ready")
	default:
		state = th.Muted.Render("running")
	}
	var meta []string
	if !s.Restored.IsZero() && c.Now.Sub(s.Restored) < restoredNew {
		meta = append(meta, paint(th.Frost, "↻ restored "+Since(c.Now, s.Restored)))
	}
	if s.Clients > 0 {
		meta = append(meta, paint(th.Green, fmt.Sprintf("● %d attached", s.Clients)))
	}
	if s.Running && !s.Created.IsZero() {
		meta = append(meta, th.Muted.Render("up "+Since(c.Now, s.Created)))
	}

	col := th.Line
	if sel {
		col = th.Amber
	}
	return deskBox(col, sel, left, badge, state, strings.Join(meta, th.Muted.Render(" · ")), d.screen(c, s, in, h-2), w, h)
}

// stale is the age of a screen kept from an earlier poll, or "".
func (d *deskTab) stale(c *Ctx, s fleetapi.DeskSession) string {
	if s.Text == "" || d.err == "" && s.Error == "" {
		return ""
	}
	return Since(c.Now, d.seen[s.Name])
}

// screen is a session's last n lines, w wide, behind a one-cell margin.
func (d *deskTab) screen(c *Ctx, s fleetapi.DeskSession, w, n int) []string {
	th := c.Theme
	if n <= 0 || w <= 0 {
		return nil
	}
	switch {
	case s.Text != "":
		lines := strings.Split(strings.TrimRight(s.Text, "\n"), "\n")
		if len(lines) > n {
			lines = lines[len(lines)-n:]
		}
		out := make([]string, len(lines))
		for i, l := range lines {
			out[i] = " " + rawLine(l, max(0, w-1))
		}
		return out
	case s.Error != "":
		return []string{" " + th.Danger.Render(oneLine(s.Error))}
	case !s.Running:
		return []string{" " + th.Muted.Render("not running")}
	case d.err != "":
		return []string{" " + th.Danger.Render(oneLine(d.err))}
	}
	return []string{" " + th.Muted.Render("…")}
}

// deskBox draws body in a box of exactly w×h, with tl and tr set into its
// top border and bl and br into its bottom one; heavy when selected.
func deskBox(col color.Color, heavy bool, tl, tr, bl, br string, body []string, w, h int) []string {
	if w < 2 || h < 2 {
		return strings.Split(fillBody(nil, w, h), "\n")
	}
	c := [6]string{"╭", "╮", "╰", "╯", "─", "│"}
	if heavy {
		c = [6]string{"┏", "┓", "┗", "┛", "━", "┃"}
	}
	bs := lipgloss.NewStyle().Foreground(col)
	in := w - 2
	out := []string{bs.Render(c[0]) + edgeLine(bs, c[4], tl, tr, in) + bs.Render(c[1])}
	for i := 0; i < h-2; i++ {
		line := ""
		if i < len(body) {
			line = body[i]
		}
		out = append(out, bs.Render(c[5])+Fit(line, in)+bs.Render(c[5]))
	}
	return append(out, bs.Render(c[2])+edgeLine(bs, c[4], bl, br, in)+bs.Render(c[3]))
}

// edgeLine is a border in cells wide with left and right set into it;
// right goes, then left is cut, when they do not fit.
func edgeLine(bs lipgloss.Style, hz, left, right string, in int) string {
	lw, rw := 0, 0
	if left != "" {
		lw = lipgloss.Width(left) + 3 // "─ left "
	}
	if right != "" {
		rw = lipgloss.Width(right) + 3 // " right ─"
	}
	if lw+rw > in {
		right, rw = "", 0
	}
	if lw > in {
		if in >= 5 {
			left = ansi.Truncate(left, in-3, "…")
			lw = lipgloss.Width(left) + 3
		} else {
			left, lw = "", 0
		}
	}
	var b strings.Builder
	if left != "" {
		b.WriteString(bs.Render(hz+" ") + left + bs.Render(" "))
	}
	b.WriteString(bs.Render(strings.Repeat(hz, max(0, in-lw-rw))))
	if right != "" {
		b.WriteString(bs.Render(" ") + right + bs.Render(" "+hz))
	}
	return b.String()
}

// deskCard is lines in a rounded box w wide with title and right set into
// its top border, each line two cells in from the sides.
func deskCard(col color.Color, title, right string, lines []string, w int) []string {
	if w < 6 {
		return nil
	}
	bs := lipgloss.NewStyle().Foreground(col)
	in := w - 2
	out := []string{bs.Render("╭") + edgeLine(bs, "─", title, right, in) + bs.Render("╮")}
	for _, l := range lines {
		out = append(out, bs.Render("│")+"  "+Fit(l, in-4)+"  "+bs.Render("│"))
	}
	return append(out, bs.Render("╰"+strings.Repeat("─", in)+"╯"))
}

// overlay sets box over the middle of bg, w×h, dimmed behind it.
func overlay(t *Theme, bg string, box []string, w, h int) string {
	lines := strings.Split(bg, "\n")
	bw := 0
	for _, l := range box {
		bw = max(bw, lipgloss.Width(l))
	}
	bw = min(bw, w)
	x, y := max(0, (w-bw)/2), max(0, (h-len(box))/2)
	dim := t.Muted.Faint(true)
	for i, l := range lines {
		plain := ansi.Strip(l)
		if i < y || i >= y+len(box) {
			lines[i] = dim.Render(plain)
			continue
		}
		lines[i] = Fit(dim.Render(ansi.Cut(plain, 0, x)), x) + Fit(box[i-y], bw) + dim.Render(ansi.Cut(plain, x+bw, w))
	}
	return fillBody(lines, w, h)
}

// emptyView is a desk with no sessions: what to press, and what a session
// is, in a card in the middle.
func (d *deskTab) emptyView(c *Ctx, w, h int) string {
	th := c.Theme
	cw := min(w-2, 62)
	if cw < 10 {
		return centerNote(th, "no sessions · n starts one", w, h)
	}
	in := cw - 6
	host := d.host()
	mid := func(s string) string { return strings.Repeat(" ", max(0, (in-lipgloss.Width(s))/2)) + s }
	lines := []string{
		"",
		mid(th.Strong.Render("No sessions on " + host + " yet")),
		mid(th.Muted.Render("press ") + lipgloss.NewStyle().Foreground(th.Amber).Bold(true).Render("n") + th.Muted.Render(" to start one")),
		"",
	}
	about := "A session runs in tmux on " + host + ", Claude Code unless you pick another command. " +
		"It keeps running when you detach, and a restart brings it back in its place, its Claude Code session resumed."
	for _, l := range strings.Split(ansi.Wordwrap(about, in, ""), "\n") {
		lines = append(lines, th.Muted.Render(l))
	}
	lines = append(lines, "")
	return center(strings.Join(deskCard(th.Line, th.Title.Render("Sessions"), "", lines, cw), "\n"), w, h)
}

// The launch form's fields.
const (
	launchDir = iota
	launchCmd
	launchCustom // only when Custom… is chosen
	launchName
)

// launchCmds is the command picker's chips: the label, a shorter one for a
// narrow form, and the command sent ("" is Claude Code's default).
var launchCmds = []struct{ label, short, cmd string }{
	{"Claude", "Claude", ""},
	{"Claude Opus", "Opus", "claude --model opus"},
	{"Claude Sonnet", "Sonnet", "claude --model sonnet"},
	{"Shell", "Shell", "shell"},
	{"Custom…", "Custom…", ""},
}

const launchCustomCmd = 4 // Custom…: the command is typed

// launchForm is n's form: a directory, completed from fleetd's machine as
// it is typed, the command and a name.
type launchForm struct {
	dir, custom, name textinput.Model
	field             int
	cmd               int      // the chosen chip
	sugg              []string // the directories offered under the field
	recent            bool     // they are the recent ones: nothing is typed
	hi, top           int      // the highlighted one, the first in view
	moved             bool     // ↑↓ chose it
	n                 int      // the directory's edits: an answer for an older one is dropped
	waiting           bool     // a lookup is due or out
	lookErr           string
	err               string // the launch's failure
	busy              bool   // the launch is out
	recents           []string
}

func newLaunchForm(t *Theme, recents []string) *launchForm {
	in := func(ph string) textinput.Model {
		m := textinput.New()
		m.Prompt, m.Placeholder = "", ph
		return m
	}
	f := &launchForm{dir: in("type a path · tab completes"), custom: in("a command, run in the directory"), name: in(""), recents: recents}
	f.dir.ShowSuggestions = true
	f.style(t)
	f.dir.Focus()
	f.offerRecent()
	return f
}

// style gives the fields the theme's colors and a steady cursor.
func (f *launchForm) style(t *Theme) {
	s := textinput.DefaultStyles(t.Dark)
	text := lipgloss.NewStyle().Foreground(t.Text)
	s.Focused.Text, s.Blurred.Text = text, text
	s.Focused.Placeholder, s.Blurred.Placeholder = t.Muted, t.Muted
	s.Focused.Suggestion, s.Blurred.Suggestion = t.Muted, t.Muted
	s.Cursor.Color, s.Cursor.Blink = t.Amber, false
	for _, m := range []*textinput.Model{&f.dir, &f.custom, &f.name} {
		m.SetStyles(s)
	}
}

// offerRecent lists the recent directories.
func (f *launchForm) offerRecent() {
	f.sugg, f.recent, f.hi, f.moved, f.waiting, f.lookErr = f.recents, true, 0, false, false, ""
	f.ghost()
}

// ghost shows the rest of the highlighted directory after what is typed.
func (f *launchForm) ghost() {
	if f.hi < len(f.sugg) {
		f.dir.SetSuggestions([]string{f.sugg[f.hi]})
	} else {
		f.dir.SetSuggestions(nil)
	}
}

func (f *launchForm) fields() []int {
	if f.cmd == launchCustomCmd {
		return []int{launchDir, launchCmd, launchCustom, launchName}
	}
	return []int{launchDir, launchCmd, launchName}
}

// next is the field after this one (by), wrapping.
func (f *launchForm) next(by int) int {
	fs := f.fields()
	i := max(0, slices.Index(fs, f.field))
	return fs[(i+by+len(fs))%len(fs)]
}

func (f *launchForm) focus(field int) tea.Cmd {
	f.field = field
	f.dir.Blur()
	f.custom.Blur()
	f.name.Blur()
	switch field {
	case launchDir:
		return f.dir.Focus()
	case launchCustom:
		return f.custom.Focus()
	case launchName:
		return f.name.Focus()
	}
	return nil
}

// formKey takes a key while the form is open.
func (d *deskTab) formKey(m tea.KeyPressMsg, c *Ctx) tea.Cmd {
	f := d.form
	k := m.String()
	switch k {
	case "esc":
		d.form = nil
		return nil
	case "ctrl+a":
		d.attach = !d.attach
		return nil
	case "enter":
		return d.launch(c)
	case "shift+tab":
		return f.focus(f.next(-1))
	}
	switch f.field {
	case launchDir:
		switch k {
		case "tab":
			if f.accept() {
				return f.lookup(c)
			}
			return f.focus(f.next(1))
		case "up", "ctrl+p":
			if f.hi > 0 {
				f.hi, f.moved = f.hi-1, true
				f.ghost()
			}
			return nil
		case "down", "ctrl+n":
			if f.hi+1 < len(f.sugg) {
				f.hi, f.moved = f.hi+1, true
				f.ghost()
				return nil
			}
			return f.focus(f.next(1))
		}
		return f.typeInto(m)
	case launchCmd:
		n := len(launchCmds)
		switch k {
		case "left", "h":
			f.cmd = (f.cmd + n - 1) % n
		case "right", "l", "space":
			f.cmd = (f.cmd + 1) % n
		case "tab", "down", "j":
			return f.focus(f.next(1))
		case "up", "k":
			return f.focus(f.next(-1))
		}
		return nil
	}
	switch k {
	case "tab", "down":
		return f.focus(f.next(1))
	case "up":
		return f.focus(f.next(-1))
	}
	return f.typeInto(m)
}

// typeInto gives a key or a paste to the field in focus; an edit of the
// directory looks again.
func (f *launchForm) typeInto(msg tea.Msg) tea.Cmd {
	in := map[int]*textinput.Model{launchDir: &f.dir, launchCustom: &f.custom, launchName: &f.name}[f.field]
	if in == nil {
		return nil
	}
	before := in.Value()
	var cmd tea.Cmd
	*in, cmd = in.Update(msg)
	if in.Value() == before {
		return cmd
	}
	f.err = ""
	if f.field == launchDir {
		return tea.Batch(cmd, f.edited())
	}
	return cmd
}

// edited follows an edit of the directory: the offers narrow to those that
// still match at once, and fleetd is asked once typing pauses.
func (f *launchForm) edited() tea.Cmd {
	f.n++
	f.moved = false
	v := f.dir.Value()
	if v == "" {
		f.offerRecent()
		return nil
	}
	f.sugg = slices.DeleteFunc(slices.Clone(f.sugg), func(s string) bool { return !completes(v, s) && !strings.EqualFold(s, v) })
	f.hi, f.waiting = 0, true
	f.ghost()
	n := f.n
	return tea.Tick(dirsWait, func(time.Time) tea.Msg { return dirsTickMsg{n: n} })
}

// accept takes the highlighted directory into the field and descends into
// it; false when there is none to take.
func (f *launchForm) accept() bool {
	if f.hi >= len(f.sugg) {
		return false
	}
	v := strings.TrimSuffix(f.sugg[f.hi], "/") + "/"
	if v == f.dir.Value() {
		return false
	}
	f.dir.SetValue(v)
	f.dir.CursorEnd()
	f.n++
	f.sugg, f.recent, f.hi, f.moved, f.waiting = nil, false, 0, false, true
	f.ghost()
	return true
}

// lookup asks fleetd for the directories that match what is typed.
func (f *launchForm) lookup(c *Ctx) tea.Cmd {
	cl, n, prefix := c.Client, f.n, f.dir.Value()
	if cl == nil {
		return nil
	}
	f.waiting = true
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
		defer cancel()
		dirs, err := cl.DeskDirs(ctx, prefix)
		return dirsMsg{n: n, dirs: dirs, err: err}
	}
}

// offered is fleetd's directories, unless typing has gone on since.
func (f *launchForm) offered(m dirsMsg) {
	if m.n != f.n {
		return
	}
	f.waiting = false
	if m.err != nil {
		f.sugg, f.lookErr = nil, m.err.Error()
	} else {
		f.sugg, f.lookErr = m.dirs, ""
	}
	f.recent, f.hi, f.top, f.moved = false, 0, 0, false
	f.ghost()
}

// target is the directory a launch goes to: the highlighted offer when ↑↓
// chose it, nothing is typed, or it finishes the name being typed; else
// what is typed (fleetd offers a directory's insides once it is typed
// whole, and ⏎ then means that directory).
func (f *launchForm) target() string {
	v := strings.TrimSpace(f.dir.Value())
	if f.hi < len(f.sugg) {
		pick := f.sugg[f.hi]
		if f.moved || v == "" || completes(v, pick) && !slices.Contains(f.sugg, v) {
			v = pick
		}
	}
	if len(v) > 1 {
		v = strings.TrimSuffix(v, "/")
	}
	return v
}

// completes: dir is beside what typed names, its name starting with the
// last part of typed, whatever the case; not a directory inside typed.
func completes(typed, dir string) bool {
	if typed == "" || strings.HasSuffix(typed, "/") || strings.HasPrefix(dir, typed+"/") {
		return false
	}
	return nameStarts(path.Base(dir), typed[strings.LastIndex(typed, "/")+1:])
}

// nameStarts: name starts with part, whatever the case.
func nameStarts(name, part string) bool {
	return len(name) >= len(part) && strings.EqualFold(name[:len(part)], part)
}

// autoName is the name fleetd gives a session in dir when none is asked
// for: the directory's, made unique as fleetd does.
func autoName(dir string, ss []fleetapi.DeskSession) string {
	base := path.Base(dir)
	taken := func(n string) bool {
		return slices.ContainsFunc(ss, func(s fleetapi.DeskSession) bool { return s.Name == n })
	}
	name := base
	for i := 2; taken(name); i++ {
		name = fmt.Sprintf("%s-%d", base, i)
	}
	return name
}

// launch sends the form: the session is made, and the form closes when
// fleetd says it is (made), or shows why not.
func (d *deskTab) launch(c *Ctx) tea.Cmd {
	f := d.form
	if f.busy {
		return nil
	}
	dir := f.target()
	if dir == "" {
		f.err = "type a directory"
		return f.focus(launchDir)
	}
	command := launchCmds[f.cmd].cmd
	if f.cmd == launchCustomCmd {
		if command = strings.TrimSpace(f.custom.Value()); command == "" {
			f.err = "type the command to run"
			return f.focus(launchCustom)
		}
	}
	req := fleetapi.DeskCreate{Name: strings.TrimSpace(f.name.Value()), Dir: dir, Command: command}
	f.busy, f.err = true, ""
	cl := c.Client
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
		defer cancel()
		s, err := cl.DeskCreate(ctx, req)
		if err == nil && s == nil {
			err = errors.New("fleetd made no session")
		}
		return deskMadeMsg{s: s, err: err}
	}
}

// formView is the launch form as a card for the middle of a w×h body.
func (d *deskTab) formView(c *Ctx, w, h int) []string {
	f, th := d.form, c.Theme
	f.style(th)
	cw := min(w-4, 84)
	if cw < 24 || h < 5 {
		return []string{Fit(th.Selected.Render("New session")+th.Muted.Render(" · enlarge me"), w)}
	}
	in := cw - 6 // inside the card's margins
	const lw = 13
	fw := max(1, in-lw)
	label := func(field int, s string) string {
		if f.field == field {
			return lipgloss.NewStyle().Foreground(th.Amber).Bold(true).Render(Fit("▸ "+s, lw))
		}
		return th.Muted.Render(Fit("  "+s, lw))
	}
	rows := min(deskSuggests, max(1, h-14))
	f.dir.SetWidth(fw - 1)
	lines := []string{"", label(launchDir, "Directory") + fieldView(f.dir.View(), fw)}
	lines = append(lines, f.offersView(th, rows, lw, fw)...)
	lines = append(lines, "", label(launchCmd, "Command")+f.chipsView(th, fw))
	if f.cmd == launchCustomCmd {
		f.custom.SetWidth(fw - 1)
		lines = append(lines, label(launchCustom, "Run")+fieldView(f.custom.View(), fw))
	}
	f.name.Placeholder = "from the directory"
	if dir := f.target(); dir != "" {
		f.name.Placeholder = autoName(dir, d.sessions())
	}
	f.name.SetWidth(fw - 1)
	lines = append(lines, "", label(launchName, "Name")+fieldView(f.name.View(), fw), "")

	switch {
	case f.busy:
		lines = append(lines, paint(th.Amber, "◌ launching…"))
	case f.err != "":
		lines = append(lines, th.Danger.Render("✕ ")+lipgloss.NewStyle().Foreground(th.Text).Render(oneLine(f.err)))
	default:
		lines = append(lines, th.Muted.Render("in tmux on "+d.host()+": it outlives detaching and restarts"))
	}
	keys := []string{hint(th, keyIs("⏎", "launch")), hint(th, keyIs("tab", "complete")), hint(th, keyIs("esc", "cancel"))}
	toggle := th.Muted.Render("○ attach on launch ") + hint(th, keyIs("^a", ""))
	if d.attach {
		toggle = paint(th.Green, "✓ ") + lipgloss.NewStyle().Foreground(th.Text).Render("attach on launch ") + hint(th, keyIs("^a", ""))
	}
	foot := strings.Join(keys, th.Muted.Render(" · "))
	if lipgloss.Width(foot)+lipgloss.Width(toggle)+2 > in {
		foot = hint(th, keyIs("⏎", "launch"))
	}
	lines = append(lines, spread(foot, toggle, in))
	return deskCard(th.Amber, th.Title.Render("New session"), th.Muted.Render("on "+d.host()), lines, cw)
}

// fieldView is an input's view in w cells: the ghost of a completion runs
// past the input's own width, into its padding, which is cut.
func fieldView(v string, w int) string {
	if lipgloss.Width(v) > w {
		v = ansi.Truncate(v, w, "")
	}
	return v
}

// offersView is the directories under the field: rows lines, lw in.
func (f *launchForm) offersView(th *Theme, rows, lw, fw int) []string {
	out := make([]string, rows)
	pad := strings.Repeat(" ", lw)
	switch {
	case f.lookErr != "":
		out[0] = pad + th.Danger.Render("✕ ") + th.Muted.Render(oneLine(f.lookErr))
		return out
	case len(f.sugg) == 0 && f.waiting:
		out[0] = pad + th.Muted.Render("…")
		return out
	case len(f.sugg) == 0 && f.dir.Value() != "":
		out[0] = pad + th.Muted.Render("no directory here starts so; ⏎ launches in what is typed")
		return out
	}
	f.top = keepInView(f.top, f.hi, len(f.sugg), rows)
	typed := f.dir.Value()
	for i := 0; i < rows && f.top+i < len(f.sugg); i++ {
		j := f.top + i
		s := f.sugg[j]
		var line string
		if j == f.hi {
			line = paint(th.Amber, "▸ ") + th.Selected.Render(s)
		} else if typed != "" && strings.HasPrefix(s, typed) {
			line = "  " + th.Muted.Render(s[:len(typed)]) + lipgloss.NewStyle().Foreground(th.Text).Render(s[len(typed):])
		} else {
			line = "  " + lipgloss.NewStyle().Foreground(th.Text).Render(s)
		}
		var tag string
		switch {
		case i == 0 && f.recent:
			tag = th.Muted.Render("recent")
		case i == rows-1 && len(f.sugg) > f.top+rows:
			tag = th.Muted.Render(fmt.Sprintf("↓ %d more", len(f.sugg)-f.top-rows))
		}
		out[i] = pad + spread(line, tag, fw)
	}
	return out
}

// chipsView is the command picker, w wide at most: the chosen chip lit.
func (f *launchForm) chipsView(th *Theme, w int) string {
	render := func(short bool) string {
		var parts []string
		for i, ch := range launchCmds {
			l := ch.label
			if short {
				l = ch.short
			}
			switch {
			case i == f.cmd && f.field == launchCmd:
				parts = append(parts, lipgloss.NewStyle().Foreground(th.Ground).Background(th.Amber).Bold(true).Render(" ◉ "+l+" "))
			case i == f.cmd:
				parts = append(parts, th.Selected.Render(" ◉ "+l+" "))
			default:
				parts = append(parts, th.Muted.Render(" ○ ")+lipgloss.NewStyle().Foreground(th.Text).Render(l+" "))
			}
		}
		return strings.Join(parts, "")
	}
	s := render(false)
	if lipgloss.Width(s) > w {
		s = render(true)
	}
	return s
}
