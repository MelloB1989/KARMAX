package tui

import (
	"context"
	"errors"
	"fmt"
	"image/color"
	"path"
	"slices"
	"strings"
	"time"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/MelloB1989/karmax/fleet/internal/fleetapi"
)

// The desk's launch form (n), and the cards it and the other questions
// are drawn in.

type (
	deskMadeMsg struct {
		s   *fleetapi.DeskSession
		err error
	}
	// dirsTickMsg is a pause in typing the directory: n is its edit count.
	dirsTickMsg struct{ n int }
	dirsMsg     struct {
		n    int
		dirs []string
		err  error
	}
)

// Launch form timing and size.
const (
	dirsWait     = 150 * time.Millisecond
	deskSuggests = 5 // directories the launch form lists at once
)

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

// The launch form's fields.
const (
	launchDir    = iota
	launchBranch // a worktree of the directory's repository (worktrunk)
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
// it is typed, a branch to work on in a worktree of its own (worktrunk),
// the command and a name.
type launchForm struct {
	dir, branch, custom, name textinput.Model
	field                     int
	cmd                       int      // the chosen chip
	sugg                      []string // the directories offered under the field
	recent                    bool     // they are the recent ones: nothing is typed
	hi, top                   int      // the highlighted one, the first in view
	moved                     bool     // ↑↓ chose it
	n                         int      // the directory's edits: an answer for an older one is dropped
	waiting                   bool     // a lookup is due or out
	lookErr                   string
	err                       string // the launch's failure
	busy                      bool   // the launch is out
	recents                   []string
}

func newLaunchForm(t *Theme, recents []string) *launchForm {
	in := func(ph string) textinput.Model {
		m := textinput.New()
		m.Prompt, m.Placeholder = "", ph
		return m
	}
	f := &launchForm{dir: in("type a path · tab completes"), branch: in("none: the directory itself · a branch: a worktree of its own"), custom: in("a command, run in the directory"), name: in(""), recents: recents}
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
	for _, m := range []*textinput.Model{&f.dir, &f.branch, &f.custom, &f.name} {
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
		return []int{launchDir, launchBranch, launchCmd, launchCustom, launchName}
	}
	return []int{launchDir, launchBranch, launchCmd, launchName}
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
	f.branch.Blur()
	f.custom.Blur()
	f.name.Blur()
	switch field {
	case launchDir:
		return f.dir.Focus()
	case launchBranch:
		return f.branch.Focus()
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
	in := map[int]*textinput.Model{launchDir: &f.dir, launchBranch: &f.branch, launchCustom: &f.custom, launchName: &f.name}[f.field]
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
	req := fleetapi.DeskCreate{Name: strings.TrimSpace(f.name.Value()), Dir: dir, Command: command, Workspace: d.active, Branch: f.branchName()}
	f.busy, f.err = true, ""
	cl := c.Client
	wait := callTimeout
	if req.Branch != "" { // worktrunk makes the worktree first, running the repository's hooks
		wait = worktreeWait
	}
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), wait)
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
	rows := min(deskSuggests, max(1, h-15))
	f.dir.SetWidth(fw - 1)
	lines := []string{"", label(launchDir, "Directory") + fieldView(f.dir.View(), fw)}
	lines = append(lines, f.offersView(th, rows, lw, fw)...)
	f.branch.SetWidth(fw - 1)
	lines = append(lines, label(launchBranch, "Branch")+fieldView(f.branch.View(), fw))
	lines = append(lines, "", label(launchCmd, "Command")+f.chipsView(th, fw))
	if f.cmd == launchCustomCmd {
		f.custom.SetWidth(fw - 1)
		lines = append(lines, label(launchCustom, "Run")+fieldView(f.custom.View(), fw))
	}
	f.name.Placeholder = "from the directory"
	if b := f.branchName(); b != "" {
		f.name.Placeholder = autoName(strings.ReplaceAll(b, "/", "-"), d.sessions())
	} else if dir := f.target(); dir != "" {
		f.name.Placeholder = autoName(dir, d.sessions())
	}
	f.name.SetWidth(fw - 1)
	lines = append(lines, "", label(launchName, "Name")+fieldView(f.name.View(), fw), "")

	switch {
	case f.busy && f.branchName() != "":
		lines = append(lines, paint(th.Amber, "◌ making the worktree, then launching…"))
	case f.busy:
		lines = append(lines, paint(th.Amber, "◌ launching…"))
	case f.err != "":
		lines = append(lines, th.Danger.Render("✕ ")+lipgloss.NewStyle().Foreground(th.Text).Render(oneLine(f.err)))
	case f.branchName() != "":
		lines = append(lines, th.Muted.Render("in a worktree of its own (worktrunk), in tmux on "+d.host()))
	default:
		lines = append(lines, th.Muted.Render("in tmux on "+d.host()+": it outlives fleetctl and restarts"))
	}
	keys := []string{hint(th, keyIs("⏎", "launch")), hint(th, keyIs("tab", "complete")), hint(th, keyIs("esc", "cancel"))}
	foot := strings.Join(keys, th.Muted.Render(" · "))
	if lipgloss.Width(foot) > in {
		foot = hint(th, keyIs("⏎", "launch"))
	}
	lines = append(lines, foot)
	return deskCard(th.Amber, th.Title.Render("New session"), th.Muted.Render("in "+d.wsName(d.active)+" · on "+d.host()), lines, cw)
}

// branchName is the branch typed, if any.
func (f *launchForm) branchName() string { return strings.TrimSpace(f.branch.Value()) }

// worktreeWait is how long a launch on a branch may take: fleetd waits up
// to DeskWorktreeWait on worktrunk.
const worktreeWait = fleetapi.DeskWorktreeWait + callTimeout

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
