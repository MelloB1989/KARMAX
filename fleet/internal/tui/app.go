package tui

import (
	"context"
	"errors"
	"fmt"
	"image/color"
	"strings"
	"time"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/MelloB1989/karmax/fleet/internal/fleetapi"
)

// Run starts the TUI in this terminal and returns when the user quits or
// ctx ends.
func Run(ctx context.Context, opts Options) error {
	if opts.Client == nil {
		return errors.New("tui: no client")
	}
	_, err := tea.NewProgram(newApp(ctx, opts), tea.WithContext(ctx)).Run()
	if ctx.Err() != nil && (errors.Is(err, tea.ErrProgramKilled) || errors.Is(err, context.Canceled)) {
		return nil
	}
	return err
}

// defaultTabs is the TUI's tabs in order; task 6's tabs are appended by name.
func defaultTabs() []Tab {
	return []Tab{newFleet(), newScreens(), newTasks(), newEvents(), newQuota(), newHosts(), newAccess()}
}

const (
	bootFrames = 24                    // the boot sweep: 24 frames …
	bootTick   = 33 * time.Millisecond // … of 33 ms, about 800 ms
	frameTick  = time.Second / 6       // sprites: about 6 fps
	toastFor   = 4 * time.Second
	minBack    = time.Second
	maxBack    = 15 * time.Second
)

// Messages only the app sends itself.
type (
	bootTickMsg  struct{}
	retryMsg     struct{}
	toastDoneMsg struct{ n int }
)

// App is the TUI's tea.Model: the frame around the tabs, the boot screen,
// tab switching and the watch loop. Tabs draw the body.
type App struct {
	opts Options
	ctx  context.Context
	c    Ctx
	tabs []Tab
	// active is the shown tab's title, so it survives the visible set
	// changing (the first snapshot says whether this is a guest).
	active  int
	w, h    int
	booting bool
	bootN   int
	help    bool
	ticking bool // a FrameMsg is on its way

	since uint64        // the last snapshot's seq
	err   error         // the watch's last failure; nil once a snapshot lands
	back  time.Duration // the current retry wait
	retry time.Time     // when the next try goes

	toast  ToastMsg
	toastN int
}

func newApp(ctx context.Context, opts Options) *App {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	a := &App{opts: opts, ctx: ctx, tabs: defaultTabs(), booting: !opts.NoAnim}
	a.c = Ctx{Client: opts.Client, Theme: NewTheme(true), Now: opts.Now(), NoAnim: opts.NoAnim}
	a.c.Attach = func(worker string, mode fleetapi.TermMode) tea.Cmd {
		if a.opts.Attach == nil {
			return toast("ssh is not available in this build", true)
		}
		return tea.Exec(a.opts.Attach(worker, mode), func(err error) tea.Msg {
			if err != nil {
				return ToastMsg{Text: worker + ": " + err.Error(), Err: true}
			}
			return ToastMsg{Text: "back from " + worker}
		})
	}
	return a
}

// toast is a command that shows text in the footer.
func toast(text string, isErr bool) tea.Cmd {
	return func() tea.Msg { return ToastMsg{Text: text, Err: isErr} }
}

func (a *App) Init() tea.Cmd {
	cmds := []tea.Cmd{tea.RequestBackgroundColor, a.watch(0)}
	if a.booting {
		cmds = append(cmds, tea.Tick(bootTick, func(time.Time) tea.Msg { return bootTickMsg{} }))
	}
	return tea.Batch(cmds...)
}

// watch is one long poll for a snapshot newer than since.
func (a *App) watch(since uint64) tea.Cmd {
	cl, ctx := a.opts.Client, a.ctx
	return func() tea.Msg {
		s, err := cl.Watch(ctx, since)
		switch {
		case ctx.Err() != nil:
			return nil
		case err != nil:
			return WatchErrMsg{Err: err}
		case s == nil:
			return WatchErrMsg{Err: errors.New("fleetd sent an empty snapshot")}
		}
		return SnapshotMsg{Snap: s}
	}
}

// me is the caller as the last snapshot says; zero before the first.
func (a *App) me() fleetapi.Me {
	if a.c.Snap == nil {
		return fleetapi.Me{}
	}
	return a.c.Snap.Me
}

// visible is the tabs this caller has, in order.
func (a *App) visible() []Tab {
	var out []Tab
	for _, t := range a.tabs {
		if t.Visible(a.me()) {
			out = append(out, t)
		}
	}
	return out
}

// current is the shown tab: the active one, or the first visible one when
// the active one is not (a guest's first snapshot).
func (a *App) current() Tab {
	if a.active < len(a.tabs) && a.tabs[a.active].Visible(a.me()) {
		return a.tabs[a.active]
	}
	for i, t := range a.tabs {
		if t.Visible(a.me()) {
			a.active = i
			return t
		}
	}
	return a.tabs[0]
}

// show makes the n-th visible tab the active one and tells it so.
func (a *App) show(n int) tea.Cmd {
	vis := a.visible()
	if n < 0 || n >= len(vis) {
		return nil
	}
	for i, t := range a.tabs {
		if t == vis[n] {
			if i == a.active {
				return nil
			}
			a.active = i
		}
	}
	return a.current().Update(FrameMsg{}, &a.c)
}

// shownIndex is the active tab's place among the visible ones.
func (a *App) shownIndex() int {
	cur := a.current()
	for i, t := range a.visible() {
		if t == cur {
			return i
		}
	}
	return 0
}

// animating: sprites move while there is a fleet to draw and animation is on.
func (a *App) animating() bool {
	if a.opts.NoAnim || a.booting || a.c.Snap == nil {
		return false
	}
	for _, w := range a.c.Snap.Workers {
		if w.Activity.Kind != fleetapi.ActDown && w.Activity.Kind != "" {
			return true
		}
	}
	return false
}

// tick starts the frame ticker unless one is already on its way.
func (a *App) tick() tea.Cmd {
	if a.ticking || !a.animating() {
		return nil
	}
	a.ticking = true
	return tea.Tick(frameTick, func(time.Time) tea.Msg { return FrameMsg{} })
}

func (a *App) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	a.c.Now = a.opts.Now()
	switch m := msg.(type) {
	case tea.WindowSizeMsg:
		a.w, a.h = m.Width, m.Height
		return a, a.current().Update(m, &a.c)

	case tea.BackgroundColorMsg:
		a.c.Theme = NewTheme(m.IsDark())
		return a, nil

	case bootTickMsg:
		if !a.booting {
			return a, nil
		}
		if a.bootN++; a.bootN >= bootFrames {
			a.booting = false
			return a, a.tick()
		}
		return a, tea.Tick(bootTick, func(time.Time) tea.Msg { return bootTickMsg{} })

	case tea.KeyPressMsg:
		return a, a.key(m)

	case SnapshotMsg:
		if m.Snap == nil {
			return a, a.watch(a.since)
		}
		a.c.Snap, a.since, a.err, a.back = m.Snap, m.Snap.Seq, nil, 0
		return a, tea.Batch(a.watch(a.since), a.current().Update(m, &a.c), a.tick())

	case WatchErrMsg:
		a.err = m.Err
		a.back = min(max(a.back*2, minBack), maxBack)
		a.retry = a.c.Now.Add(a.back)
		return a, tea.Batch(a.current().Update(m, &a.c), tea.Tick(a.back, func(time.Time) tea.Msg { return retryMsg{} }))

	case retryMsg:
		return a, a.watch(a.since)

	case FrameMsg:
		a.ticking = false
		if !a.opts.NoAnim {
			a.c.Frame++
		}
		return a, tea.Batch(a.current().Update(m, &a.c), a.tick())

	case ToastMsg:
		a.toast, a.toastN = m, a.toastN+1
		n := a.toastN
		return a, tea.Batch(a.current().Update(m, &a.c), tea.Tick(toastFor, func(time.Time) tea.Msg { return toastDoneMsg{n} }))

	case toastDoneMsg:
		if m.n == a.toastN {
			a.toast = ToastMsg{}
		}
		return a, nil
	}
	return a, a.current().Update(msg, &a.c)
}

// key handles a key: the app's own keys unless the tab has a field
// focused, then the tab's.
func (a *App) key(m tea.KeyPressMsg) tea.Cmd {
	k := m.String()
	if k == "ctrl+c" {
		return tea.Quit
	}
	if a.booting {
		a.booting = false
		return a.tick()
	}
	if a.help {
		a.help = false
		return nil
	}
	tab := a.current()
	if !tab.Capturing() {
		n := len(a.visible())
		switch k {
		case "q":
			return tea.Quit
		case "?":
			a.help = true
			return nil
		case "tab":
			return a.show((a.shownIndex() + 1) % n)
		case "shift+tab":
			return a.show((a.shownIndex() + n - 1) % n)
		case "1", "2", "3", "4", "5", "6", "7", "8", "9":
			return a.show(int(k[0] - '1'))
		}
	}
	return tab.Update(m, &a.c)
}

func (a *App) View() tea.View {
	v := tea.NewView(a.render())
	v.AltScreen = true
	v.WindowTitle = "KARMAX ▸ fleetctl"
	return v
}

// The frame's fixed rows: top border, tab bar, rule, rule, footer, bottom.
const frameRows = 6

func (a *App) render() string {
	w, h := a.w, a.h
	if w <= 0 || h <= 0 {
		return ""
	}
	if a.booting {
		return a.bootView(w, h)
	}
	if w < 24 || h < frameRows+2 {
		return a.tinyView(w, h)
	}
	t := a.c.Theme
	edge := lipgloss.NewStyle().Foreground(t.Grey)
	side := edge.Render("│")
	row := func(s string) string { return side + " " + Fit(s, w-4) + " " + side }

	bw, bh := w-4, h-frameRows
	var body []string
	if a.err != nil && a.c.Snap != nil {
		body = append(body, a.banner(bw))
		bh--
	}
	body = append(body, strings.Split(a.body(bw, bh), "\n")...)

	out := make([]string, 0, h)
	out = append(out, a.top(w), row(a.tabBar(bw)), edge.Render("├"+strings.Repeat("─", w-2)+"┤"))
	for _, l := range body {
		out = append(out, row(l))
	}
	out = append(out, edge.Render("├"+strings.Repeat("─", w-2)+"┤"), row(a.footer(bw)), edge.Render("╰"+strings.Repeat("─", w-2)+"╯"))
	return strings.Join(out, "\n")
}

// body is the shown tab's view (or help, or a wait for the first snapshot),
// exactly w×h; dimmed while fleetd is unreachable.
func (a *App) body(w, h int) string {
	t := a.c.Theme
	switch {
	case a.help:
		return a.helpView(w, h)
	case a.c.Snap == nil && a.err != nil:
		return center(t.Danger.Render("fleetd unreachable")+"\n\n"+t.Muted.Render(oneLine(a.err.Error()))+"\n"+
			t.Muted.Render("trying again in "+Age(a.retry.Sub(a.c.Now).Round(time.Second))), w, h)
	case a.c.Snap == nil:
		return center(t.Gradient("KARMAX", true)+"\n\n"+t.Muted.Render("connecting to fleetd…"), w, h)
	}
	s := block(a.current().View(&a.c, w, h), w, h)
	if a.err == nil {
		return s
	}
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = t.Muted.Faint(true).Render(ansi.Strip(l))
	}
	return strings.Join(lines, "\n")
}

// banner is the line over a stale snapshot while fleetd is unreachable.
func (a *App) banner(w int) string {
	t := a.c.Theme
	at := ""
	if a.c.Snap != nil {
		at = " · showing " + a.c.Snap.At.Format("15:04:05")
	}
	return t.Danger.Render("▲ fleetd unreachable") + t.Muted.Render(" · retrying in "+Age(a.retry.Sub(a.c.Now).Round(time.Second))+at+" · "+oneLine(a.err.Error()))
}

// top is the top border with the brand, who you are, the counts and the clock.
func (a *App) top(w int) string {
	t := a.c.Theme
	edge := lipgloss.NewStyle().Foreground(t.Grey)
	title := t.Gradient("KARMAX", true) + t.Muted.Render(" ▸ ") + t.Strong.Render("fleetctl")
	// Who you are, the counts and the clock; the counts go first when
	// there is no room, then who you are.
	var me, counts string
	if s := a.c.Snap; s != nil {
		working := 0
		for _, w := range s.Workers {
			if w.State == "working" {
				working++
			}
		}
		noun := "workers"
		if len(s.Workers) == 1 {
			noun = "worker"
		}
		counts = t.Muted.Render(fmt.Sprintf("%d %s · ", len(s.Workers), noun)) + paint(t.Amber, fmt.Sprintf("%d working", working))
		role := lipgloss.NewStyle().Foreground(t.Amber).Bold(true)
		if s.Me.Role == fleetapi.RoleGuest {
			role = role.Foreground(t.Violet)
		}
		who := s.Me.Name
		if s.Me.Device != "" {
			who, _, _ = strings.Cut(s.Me.Device, ".")
		}
		me = role.Render(string(s.Me.Role)) + t.Muted.Render(" · "+who)
	}
	clock := t.Strong.Render(a.c.Now.Format("15:04"))
	var tries [][]string
	for _, segs := range [][]string{{me, counts, clock}, {me, clock}, {clock}, {}} {
		var keep []string
		for _, s := range segs {
			if s != "" {
				keep = append(keep, s)
			}
		}
		tries = append(tries, keep)
	}
	for _, segs := range tries {
		right := strings.Join(segs, edge.Render(" ── "))
		fill := w - 4 - lipgloss.Width(title) - lipgloss.Width(right) - 4
		if len(segs) == 0 {
			fill = w - 5 - lipgloss.Width(title)
		}
		switch {
		case fill >= 1 && len(segs) > 0:
			return edge.Render("╭─ ") + title + edge.Render(" "+strings.Repeat("─", fill)+" ") + right + edge.Render(" ─╮")
		case fill >= 1:
			return edge.Render("╭─ ") + title + edge.Render(" "+strings.Repeat("─", fill-1)+"╮")
		}
	}
	return edge.Render("╭─ ") + Fit(title, w-5) + edge.Render(" ╮")
}

// tabBar is the numbered tabs, the shown one lit, and the link state.
// When they do not fit, the tabs close up.
func (a *App) tabBar(w int) string {
	t := a.c.Theme
	state := paint(t.Green, "● ") + t.Muted.Render("live")
	switch {
	case a.err != nil:
		state = paint(t.Alert, "● ") + t.Muted.Render("offline")
	case a.c.Snap == nil:
		state = t.Muted.Render("○ connecting")
	}
	tabs := func(compact bool) string {
		cur := a.current()
		var b strings.Builder
		for i, tb := range a.visible() {
			if i > 0 && !compact {
				b.WriteString(" ")
			}
			switch {
			case tb == cur:
				b.WriteString(lipgloss.NewStyle().Foreground(t.Ground).Background(t.Amber).Bold(true).Render(fmt.Sprintf(" %d %s ", i+1, tb.Title())))
			case compact:
				b.WriteString(t.Muted.Render(fmt.Sprintf(" %d ", i+1)) + lipgloss.NewStyle().Foreground(t.Text).Render(tb.Title()))
			default:
				b.WriteString(t.Muted.Render(fmt.Sprintf(" %d ", i+1)) + lipgloss.NewStyle().Foreground(t.Text).Render(tb.Title()+" "))
			}
			if tb.Title() == "Access" && a.c.Snap != nil && a.c.Snap.Knocks > 0 {
				badge := fmt.Sprintf("●%d", a.c.Snap.Knocks)
				if compact && tb != cur {
					badge = " " + badge
				}
				b.WriteString(paint(t.Violet, badge))
			}
		}
		return b.String()
	}
	bar := tabs(false)
	if lipgloss.Width(bar)+lipgloss.Width(state)+1 > w {
		bar = tabs(true)
	}
	if lipgloss.Width(bar)+lipgloss.Width(state)+1 > w {
		state = strings.Fields(ansi.Strip(state))[0]
		state = paint(map[bool]color.Color{true: t.Alert, false: t.Green}[a.err != nil], state)
	}
	return spread(bar, state, w)
}

// footer is the tab's keys (or a toast), and the maker's mark.
func (a *App) footer(w int) string {
	t := a.c.Theme
	mark := t.Muted.Render("made by ") + t.Gradient("MelloB", true)
	if a.opts.Version != "" {
		mark += t.Muted.Render(" · " + a.opts.Version)
	}
	var left string
	switch {
	case a.toast.Text != "" && a.toast.Err:
		left = t.Danger.Render("✕ ") + lipgloss.NewStyle().Foreground(t.Text).Render(oneLine(a.toast.Text))
	case a.toast.Text != "":
		left = lipgloss.NewStyle().Foreground(t.Green).Bold(true).Render("✓ ") + lipgloss.NewStyle().Foreground(t.Text).Render(oneLine(a.toast.Text))
	default:
		var hints []string
		if a.c.Snap != nil && !a.help {
			for _, b := range a.current().Help() {
				if b.Enabled() {
					hints = append(hints, hint(t, b))
				}
			}
		}
		// While a tab holds the keys, ? and q are its text.
		if a.help || !a.current().Capturing() {
			hints = append(hints, hint(t, keyIs("?", "help")), hint(t, keyIs("q", "quit")))
		}
		left = strings.Join(hints, t.Muted.Render(" · "))
	}
	if lipgloss.Width(mark)+2 > w {
		return left
	}
	return spread(Fit(left, w-lipgloss.Width(mark)-2), mark, w)
}

func hint(t *Theme, b key.Binding) string {
	h := b.Help()
	return lipgloss.NewStyle().Foreground(t.Text).Bold(true).Render(h.Key) + " " + t.Muted.Render(h.Desc)
}

// helpView is every key: the app's, then the shown tab's.
func (a *App) helpView(w, h int) string {
	t := a.c.Theme
	row := func(k, d string) string {
		return "  " + lipgloss.NewStyle().Foreground(t.Amber).Bold(true).Render(Fit(k, 12)) + lipgloss.NewStyle().Foreground(t.Text).Render(d)
	}
	lines := []string{t.Gradient("KARMAX", true) + t.Muted.Render(" ▸ ") + t.Strong.Render("fleetctl") + t.Muted.Render("  made by MelloB · "+a.opts.Version), "",
		t.Title.Render("Everywhere"),
		row(fmt.Sprintf("1–%d, tab", len(a.visible())), "switch tabs (shift+tab goes back)"),
		row("?", "this help; any key closes it"),
		row("q, ctrl+c", "quit"), "",
		t.Title.Render(a.current().Title())}
	for _, b := range a.current().Help() {
		lines = append(lines, row(b.Help().Key, b.Help().Desc))
	}
	lines = append(lines, "", t.Muted.Render("  ⏎ and esc open and close; a form sends with ctrl+s"))
	return block(strings.Join(lines, "\n"), w, h)
}

// tinyView is a terminal too small for the frame: the brand and a hint.
func (a *App) tinyView(w, h int) string {
	t := a.c.Theme
	return block(center(t.Gradient("KARMAX", true)+"\n"+t.Muted.Render("enlarge me"), w, h), w, h)
}

// The KARMAX wordmark in block letters, five rows.
var wordmark = []string{
	"██  ██  ████  █████  ██   ██  ████  ██  ██",
	"██ ██  ██  ██ ██  ██ ███ ███ ██  ██  ████ ",
	"████   ██████ █████  ██ █ ██ ██████   ██  ",
	"██ ██  ██  ██ ██ ██  ██   ██ ██  ██  ████ ",
	"██  ██ ██  ██ ██  ██ ██   ██ ██  ██ ██  ██",
}

// bootView is the wordmark swept in, left to right, with a bright edge,
// then the product line under it.
func (a *App) bootView(w, h int) string {
	t := a.c.Theme
	ww := lipgloss.Width(wordmark[0])
	if w < ww+2 || h < len(wordmark)+4 {
		return block(center(t.Gradient("KARMAX", true), w, h), w, h)
	}
	edge := (a.bootN + 1) * (ww + 6) / bootFrames // the sweep's leading column
	cols := lipgloss.Blend1D(ww, t.Brand...)
	shine := lipgloss.NewStyle().Foreground(t.Text).Bold(true)
	var rows []string
	for _, line := range wordmark {
		var b strings.Builder
		for x, r := range []rune(line) {
			switch {
			case x > edge || r == ' ':
				b.WriteRune(' ')
			case x >= edge-2:
				b.WriteString(shine.Render(string(r)))
			default:
				b.WriteString(lipgloss.NewStyle().Foreground(cols[x]).Render(string(r)))
			}
		}
		rows = append(rows, b.String())
	}
	sub, by := "", ""
	if a.bootN*2 >= bootFrames {
		sub = t.Strong.Render("f l e e t c t l")
	}
	if a.bootN*4 >= bootFrames*3 {
		by = t.Muted.Render("made by ") + t.Gradient("MelloB", true)
	}
	rows = append(rows, "", sub, by)
	return center(strings.Join(rows, "\n"), w, h)
}

// block cuts or pads s to exactly w×h cells.
func block(s string, w, h int) string {
	if w <= 0 || h <= 0 {
		return ""
	}
	lines := strings.Split(s, "\n")
	out := make([]string, h)
	for i := range out {
		l := ""
		if i < len(lines) {
			l = lines[i]
		}
		out[i] = Fit(l, w)
	}
	return strings.Join(out, "\n")
}

// center puts s's lines in the middle of a w×h block.
func center(s string, w, h int) string {
	lines := strings.Split(s, "\n")
	top := max(0, (h-len(lines))/2)
	out := make([]string, 0, h)
	for range top {
		out = append(out, "")
	}
	for _, l := range lines {
		out = append(out, strings.Repeat(" ", max(0, (w-lipgloss.Width(l))/2))+l)
	}
	return block(strings.Join(out, "\n"), w, h)
}

// spread puts left and right on one line of w cells, left cut to fit.
func spread(left, right string, w int) string {
	rw := lipgloss.Width(right)
	if rw >= w {
		return Fit(right, w)
	}
	return Fit(left, w-rw-1) + " " + right
}

// oneLine is s's first line, trimmed.
func oneLine(s string) string {
	s, _, _ = strings.Cut(strings.TrimSpace(s), "\n")
	return s
}

// paint is s in color c.
func paint(c color.Color, s string) string { return lipgloss.NewStyle().Foreground(c).Render(s) }
