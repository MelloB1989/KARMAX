package tui

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	uv "github.com/charmbracelet/ultraviolet"

	"github.com/MelloB1989/karmax/fleet/internal/fleetapi"
)

// The Sessions tab is the desk as workspaces, cmux's way, in the terminal:
// a sidebar of workspaces (⌘B, or b, hides it), and the chosen one's
// sessions live, in its layout. Keys go to the focused session; Ctrl-]
// steps out to move around (and back with ⏎). A file pasted or dropped
// here, or an image on the clipboard (Ctrl+V), goes to the session's
// machine and is typed there as its path.

type deskMode int

const (
	navMode  deskMode = iota // keys move around the desk
	typeMode                 // keys go to the focused session
)

// deskRetry bounds the long poll's backoff.
const deskRetry = 15 * time.Second

type (
	deskListMsg struct {
		list *fleetapi.DeskList
		err  error
	}
	deskRetryMsg struct{}
	// deskDoneMsg is a change to the desk sent: what to say, and a session
	// or workspace to show once the desk has it.
	deskDoneMsg struct {
		say    string
		err    error
		focus  string // a session
		active string // a workspace
	}
)

// sideRow is a row of the sidebar: a workspace's card, or a session of the
// shown workspace.
type sideRow struct {
	ws      string
	session string // "" for the workspace's card
}

// hit is a place on screen the mouse may click.
type hit struct {
	b       box
	ws      string // a workspace's card
	session string // a session's row, tab or pane
	pane    bool   // the session's terminal: clicks go into it
	term    box    // its terminal's cells, for a pane
	layout  bool   // the layout's name: the next layout
}

type deskTab struct {
	mgr *termMgr

	list    *fleetapi.DeskList
	err     string // the last poll's failure; "" once one answers
	polling atomic.Bool
	pollAt  time.Time // when the poll out was sent
	retryAt time.Time
	back    time.Duration

	mode     deskMode
	active   string            // the workspace shown
	focus    map[string]string // each workspace's focused session
	sidebar  bool
	cursor   int // navMode: the sidebar row
	zoom     bool
	unread   map[string]string // a session's news while not seen: finished | waiting
	statuses map[string]string // each session's last status
	w, h     int

	// What View drew, for the mouse and the cursor.
	hits      []hit
	termBoxes map[string]box
	drag      string // the session a mouse button went down in

	// The questions: one at a time.
	form    *launchForm
	confirm *confirm
	ask     *askLine
	pick    *wsPick

	// What Claude Code said, as notices: the newest first.
	notices   []notice
	noteAt    map[string]time.Time // each session's latest note taken
	hooked    map[string]bool      // its Claude Code has hooks: its notes are the news, not its status
	termFocus bool                 // the terminal window has the focus (until told otherwise)
	viewedAt  time.Time            // View last drew the tab: it is on screen
	panel     *noticePanel         // the notices, listed (!)
	recent    []string
	// want is a workspace just made: shown once the desk has it.
	want   string
	state  string // where the window's choices are kept ("" nowhere)
	loaded bool
	toast  ToastMsg
	toastT time.Time
}

func newSessions() Tab {
	return &deskTab{focus: map[string]string{}, unread: map[string]string{}, statuses: map[string]string{}, sidebar: true,
		noteAt: map[string]time.Time{}, hooked: map[string]bool{}, termFocus: true}
}

// notice is something a session's Claude Code said: it needs the operator,
// or its turn ended.
type notice struct {
	at      time.Time
	session string
	ws      string
	kind    string // fleetapi.NoteNeeds | NoteDone
	text    string
}

// maxNotices is how many notices are kept.
const maxNotices = 50

func (d *deskTab) Title() string               { return "Sessions" }
func (d *deskTab) Visible(me fleetapi.Me) bool { return me.Role == fleetapi.RoleAdmin }
func (d *deskTab) Capturing() bool {
	return d.form != nil || d.confirm != nil || d.ask != nil || d.pick != nil || d.panel != nil
}

// ReportFocus: the terminal says when its window gains and loses the focus,
// so that a session on screen in a window not looked at is still news.
func (d *deskTab) ReportFocus() bool { return true }

// Background: the desk keeps listening while another tab is shown, so that
// a session that needs the operator is said wherever they are.
func (d *deskTab) Background(msg tea.Msg) bool {
	switch msg.(type) {
	case deskListMsg, deskRetryMsg, deskPaintMsg, tea.FocusMsg, tea.BlurMsg:
		return true
	}
	return false
}

// Wake starts the desk's long poll (the app calls it as snapshots come,
// for an admin).
func (d *deskTab) Wake(c *Ctx) tea.Cmd { return d.Update(deskRetryMsg{}, c) }

// Raw: while typing into a session every key is the session's, Ctrl-C and
// the digits too.
func (d *deskTab) Raw() bool { return d.mode == typeMode && !d.Capturing() }

// Fullscreen: the desk takes the whole terminal, as a multiplexer does.
func (d *deskTab) Fullscreen() bool { return true }

// Mouse: clicks choose, the wheel scrolls a session's history.
func (d *deskTab) Mouse() bool { return true }

var deskKeys = struct{ Type, Nav, Move, New, Workspace, Layout, Rename, MoveTo, End, Zoom, Sidebar, Order key.Binding }{
	Type:      key.NewBinding(key.WithKeys("enter", "i"), key.WithHelp("⏎", "type in it")),
	Nav:       keyIs("ctrl+]", "step out (typing)"),
	Move:      key.NewBinding(key.WithKeys("up", "down", "k", "j"), key.WithHelp("↑↓", "workspaces, sessions")),
	New:       keyIs("n", "new session"),
	Workspace: keyIs("W", "new workspace"),
	Layout:    keyIs("l", "layout"),
	Rename:    keyIs("r", "rename"),
	MoveTo:    keyIs("m", "move to workspace"),
	End:       keyIs("x", "end"),
	Zoom:      keyIs("z", "zoom"),
	Sidebar:   key.NewBinding(key.WithKeys("b"), key.WithHelp("b/⌘B", "sidebar")),
	Order:     key.NewBinding(key.WithKeys("K", "J"), key.WithHelp("⇧↑↓", "reorder")),
}

func (d *deskTab) Help() []key.Binding {
	k := deskKeys
	return []key.Binding{k.Type, k.Nav, k.Move, k.New, k.Workspace, k.Layout, k.Rename, k.MoveTo, k.End, k.Zoom, k.Sidebar, k.Order,
		keyIs("[ ]", "sessions"), keyIs("{ }", "workspaces"), keyIs("alt+1–9", "workspace N"), keyIs("ctrl+v", "paste a file or image (typing)")}
}

// ---- the desk as known ----

func (d *deskTab) sessions() []fleetapi.DeskSession {
	if d.list == nil {
		return nil
	}
	return d.list.Sessions
}

func (d *deskTab) host() string {
	if d.list == nil || d.list.Host == "" {
		return "fleetd's machine"
	}
	return d.list.Host
}

func (d *deskTab) workspaces() []fleetapi.DeskWorkspace {
	if d.list == nil {
		return nil
	}
	return d.list.Workspaces
}

func (d *deskTab) ws(id string) (fleetapi.DeskWorkspace, bool) {
	for _, w := range d.workspaces() {
		if w.ID == id {
			return w, true
		}
	}
	return fleetapi.DeskWorkspace{}, false
}

func (d *deskTab) wsName(id string) string {
	if w, ok := d.ws(id); ok {
		return w.Name
	}
	return "the workspace"
}

func (d *deskTab) session(name string) (fleetapi.DeskSession, bool) {
	for _, s := range d.sessions() {
		if s.Name == name {
			return s, true
		}
	}
	return fleetapi.DeskSession{}, false
}

// in is workspace id's sessions, in order.
func (d *deskTab) in(id string) []fleetapi.DeskSession {
	var out []fleetapi.DeskSession
	for _, s := range d.sessions() {
		if s.Workspace == id {
			out = append(out, s)
		}
	}
	return out
}

// focused is the shown workspace's focused session, "" for none.
func (d *deskTab) focused() string { return d.focus[d.active] }

// shownName is how a session is named on screen: the label the operator
// gave it, else what Claude Code calls it, else tmux's name.
func shownName(s fleetapi.DeskSession) string {
	switch {
	case s.Label != "":
		return s.Label
	case s.Title != "" && s.Title != "Claude Code":
		return s.Title
	}
	return s.Name
}

// settle keeps the shown workspace and each focus on something that is
// there.
func (d *deskTab) settle() {
	wss := d.workspaces()
	if len(wss) == 0 {
		return
	}
	if _, ok := d.ws(d.active); !ok {
		d.active = wss[0].ID
	}
	for _, w := range wss {
		ss := d.in(w.ID)
		if f := d.focus[w.ID]; !slices.ContainsFunc(ss, func(s fleetapi.DeskSession) bool { return s.Name == f }) {
			if len(ss) > 0 {
				d.focus[w.ID] = ss[0].Name
			} else {
				delete(d.focus, w.ID)
			}
		}
	}
	if d.focused() == "" && d.mode == typeMode {
		d.mode = navMode
	}
	rows := d.rows()
	d.cursor = min(max(d.cursor, 0), max(len(rows)-1, 0))
}

// rows is the sidebar's rows: each workspace, the shown one's sessions
// under it.
func (d *deskTab) rows() []sideRow {
	var out []sideRow
	for _, w := range d.workspaces() {
		out = append(out, sideRow{ws: w.ID})
		if w.ID == d.active {
			for _, s := range d.in(w.ID) {
				out = append(out, sideRow{ws: w.ID, session: s.Name})
			}
		}
	}
	return out
}

// syncCursor puts the sidebar's cursor on the focused session, else on the
// shown workspace.
func (d *deskTab) syncCursor() {
	for i, r := range d.rows() {
		if r.ws == d.active && r.session == d.focused() {
			d.cursor = i
			return
		}
	}
	for i, r := range d.rows() {
		if r.ws == d.active && r.session == "" {
			d.cursor = i
			return
		}
	}
}

// ---- the window's choices, kept ----

type deskUI struct {
	Active  string            `json:"active"`
	Focus   map[string]string `json:"focus"`
	Sidebar bool              `json:"sidebar"`
}

func (d *deskTab) loadUI(c *Ctx) {
	if d.loaded {
		return
	}
	d.loaded, d.state = true, c.DeskState
	if d.state == "" {
		return
	}
	b, err := os.ReadFile(d.state)
	if err != nil {
		return
	}
	var u deskUI
	if json.Unmarshal(b, &u) != nil {
		return
	}
	d.active, d.sidebar = u.Active, u.Sidebar
	for k, v := range u.Focus {
		d.focus[k] = v
	}
}

func (d *deskTab) saveUI() {
	if d.state == "" {
		return
	}
	b, _ := json.Marshal(deskUI{Active: d.active, Focus: d.focus, Sidebar: d.sidebar})
	os.MkdirAll(filepath.Dir(d.state), 0o700)
	tmp := d.state + ".new"
	if os.WriteFile(tmp, b, 0o600) == nil {
		os.Rename(tmp, d.state)
	}
}

// ---- update ----

func (d *deskTab) Update(msg tea.Msg, c *Ctx) tea.Cmd {
	d.loadUI(c)
	if d.mgr == nil && c.Client != nil {
		d.mgr = newTermMgr(context.Background(), c.Client)
	}
	var cmds []tea.Cmd
	switch m := msg.(type) {
	case tea.WindowSizeMsg:
		d.w, d.h = m.Width, m.Height
	case deskListMsg:
		cmds = append(cmds, d.take(m, c))
	case deskRetryMsg:
	case deskPaintMsg:
		for _, s := range m.clips {
			cmds = append(cmds, tea.SetClipboard(s), toastCmd(fmt.Sprintf("copied %d characters", len([]rune(s))), false))
		}
	case deskDoneMsg:
		if m.err != nil {
			cmds = append(cmds, toastCmd(m.err.Error(), true))
		} else {
			if m.active != "" {
				if _, ok := d.ws(m.active); ok {
					d.active = m.active
				} else {
					d.want = m.active
				}
			}
			if m.focus != "" {
				if s, ok := d.session(m.focus); ok {
					d.active, d.focus[s.Workspace] = s.Workspace, s.Name
				}
			}
			if m.say != "" {
				cmds = append(cmds, toastCmd(m.say, false))
			}
		}
	case deskMadeMsg:
		cmds = append(cmds, d.made(m))
	case deskPastedMsg:
		cmds = append(cmds, d.pasted(m))
	case dirsTickMsg:
		if d.form != nil && m.n == d.form.n {
			cmds = append(cmds, d.form.lookup(c))
		}
	case dirsMsg:
		if d.form != nil {
			d.form.offered(m)
		}
	case ToastMsg:
		d.toast, d.toastT = m, c.Now
	case tea.FocusMsg:
		d.termFocus = true
	case tea.BlurMsg:
		d.termFocus = false
	case tea.KeyPressMsg:
		cmds = append(cmds, d.key(m, c))
	case tea.PasteMsg:
		cmds = append(cmds, d.paste(m, c))
	case tea.MouseClickMsg:
		cmds = append(cmds, d.click(m.Mouse(), c))
	case tea.MouseReleaseMsg:
		d.mouseTo(d.drag, uv.MouseReleaseEvent(toUV(m.Mouse())))
		d.drag = ""
	case tea.MouseMotionMsg:
		if d.drag != "" {
			d.mouseTo(d.drag, uv.MouseMotionEvent(toUV(m.Mouse())))
		}
	case tea.MouseWheelMsg:
		mm := m.Mouse()
		for _, h := range d.hits {
			if h.pane && h.term.in(mm.X, mm.Y) {
				d.mouseTo(h.session, uv.MouseWheelEvent(toUV(mm)))
				break
			}
		}
	}
	d.settle()
	d.arrange(c)
	if d.mgr != nil {
		cmds = append(cmds, d.mgr.wait())
	}
	cmds = append(cmds, d.poll(c))
	return tea.Batch(cmds...)
}

func toUV(m tea.Mouse) uv.Mouse {
	return uv.Mouse{X: m.X, Y: m.Y, Button: uv.MouseButton(m.Button), Mod: m.Mod}
}

// mouseTo gives a mouse event to session's program, at its terminal's
// cells.
func (d *deskTab) mouseTo(session string, ev uv.MouseEvent) {
	b, ok := d.termBoxes[session]
	if !ok || d.mgr == nil {
		return
	}
	t := d.mgr.peek(session)
	if t == nil {
		return
	}
	shift := func(m uv.Mouse) uv.Mouse {
		m.X, m.Y = min(max(m.X-b.x, 0), b.w-1), min(max(m.Y-b.y, 0), b.h-1)
		return m
	}
	switch e := ev.(type) {
	case uv.MouseClickEvent:
		t.mouse(uv.MouseClickEvent(shift(uv.Mouse(e))))
	case uv.MouseReleaseEvent:
		t.mouse(uv.MouseReleaseEvent(shift(uv.Mouse(e))))
	case uv.MouseMotionEvent:
		t.mouse(uv.MouseMotionEvent(shift(uv.Mouse(e))))
	case uv.MouseWheelEvent:
		t.mouse(uv.MouseWheelEvent(shift(uv.Mouse(e))))
	}
}

// poll is the long poll for the desk's changes: one at a time, again at
// once after an answer, after a backoff after a failure.
func (d *deskTab) poll(c *Ctx) tea.Cmd {
	if c.Client == nil || c.Now.Before(d.retryAt) {
		return nil
	}
	if !d.polling.CompareAndSwap(false, true) {
		if c.Now.Sub(d.pollAt) < 2*time.Minute { // out; one older is lost
			return nil
		}
	}
	d.pollAt = c.Now
	since := ""
	if d.list != nil {
		since = d.list.Version
	}
	cl := c.Client
	return func() tea.Msg {
		defer d.polling.Store(false) // an answer another tab got is lost: the next Update asks again
		l, err := cl.DeskWatch(context.Background(), since)
		return deskListMsg{list: l, err: err}
	}
}

// take is the desk as fleetd now says: what finished or asks for the
// operator while not seen is marked, and said.
func (d *deskTab) take(m deskListMsg, c *Ctx) tea.Cmd {
	if m.err != nil || m.list == nil {
		d.err = "fleetd sent no desk"
		if m.err != nil {
			d.err = oneLine(m.err.Error())
		}
		d.back = min(max(d.back*2, time.Second), deskRetry)
		d.retryAt = c.Now.Add(d.back)
		return tea.Tick(d.back, func(time.Time) tea.Msg { return deskRetryMsg{} })
	}
	d.err, d.back = "", 0
	first := d.list == nil
	d.list = m.list
	if _, ok := d.ws(d.want); ok {
		d.active, d.want = d.want, ""
		d.syncCursor()
	}
	if first && d.active == "" && len(m.list.Workspaces) > 0 {
		d.active = m.list.Workspaces[0].ID
	}
	return d.news(m.list, first, c)
}

// news is what changed for the operator since the last list: a note from a
// session's Claude Code (its hooks), or, for one without hooks, its status
// (asking, or done working). A notice for a session on screen, in a window
// looked at, is seen already; any other is marked, kept, said in the
// footer and sent to the desktop.
func (d *deskTab) news(l *fleetapi.DeskList, first bool, c *Ctx) tea.Cmd {
	looked := d.termFocus && c.Now.Sub(d.viewedAt) < 2*time.Second
	shown := d.shownSet()
	var fresh []notice
	seen := map[string]bool{}
	for _, s := range l.Sessions {
		seen[s.Name] = true
		was, now := d.statuses[s.Name], s.Status
		d.statuses[s.Name] = now
		if s.Note != nil {
			d.hooked[s.Name] = true
			if s.Note.At.After(d.noteAt[s.Name]) {
				d.noteAt[s.Name] = s.Note.At
				if !first {
					fresh = append(fresh, notice{at: c.Now, session: s.Name, ws: s.Workspace, kind: s.Note.Kind, text: s.Note.Text})
				}
			}
			continue
		}
		if first || was == now || d.hooked[s.Name] {
			continue
		}
		switch {
		case now == fleetapi.StatusWaiting:
			fresh = append(fresh, notice{at: c.Now, session: s.Name, ws: s.Workspace, kind: fleetapi.NoteNeeds, text: "needs you"})
		case was == fleetapi.StatusWorking && now == fleetapi.StatusIdle:
			fresh = append(fresh, notice{at: c.Now, session: s.Name, ws: s.Workspace, kind: fleetapi.NoteDone, text: "finished its turn"})
		}
	}
	for n := range d.statuses {
		if !seen[n] {
			delete(d.statuses, n)
			delete(d.unread, n)
			delete(d.noteAt, n)
			delete(d.hooked, n)
		}
	}
	var cmds []tea.Cmd
	var said []string
	for _, n := range fresh {
		if shown[n.session] && looked {
			continue // the operator is looking at it
		}
		d.unread[n.session] = n.kind
		d.notices = append([]notice{n}, d.notices...)
		s, _ := d.session(n.session)
		said = append(said, shownName(s)+" "+n.text)
		cmds = append(cmds, notify("Claude · "+shownName(s), n.text+" · "+d.wsName(n.ws)))
	}
	if len(d.notices) > maxNotices {
		d.notices = d.notices[:maxNotices]
	}
	if len(said) > 0 {
		cmds = append(cmds, toastCmd(strings.Join(said, " · ")+" · u jumps there", false))
	}
	return tea.Batch(cmds...)
}

// jump goes to the session the newest notice is about that still waits to
// be seen (else the newest at all), and types in it.
func (d *deskTab) jump() tea.Cmd {
	pick := -1
	for i, n := range d.notices {
		if _, ok := d.session(n.session); !ok {
			continue
		}
		if d.unread[n.session] != "" {
			pick = i
			break
		}
		if pick < 0 {
			pick = i
		}
	}
	if pick < 0 {
		return toastCmd("nothing new from Claude", false)
	}
	d.goTo(d.notices[pick].session)
	return nil
}

// goTo shows session name in its workspace, and types in it.
func (d *deskTab) goTo(name string) {
	s, ok := d.session(name)
	if !ok {
		return
	}
	d.active, d.focus[s.Workspace], d.zoom = s.Workspace, s.Name, false
	d.mode = typeMode
	delete(d.unread, s.Name)
	d.saveUI()
}

// notify is a desktop notice through the terminal (OSC 777, which cmux,
// Ghostty, iTerm2 and others take): a session needs the operator.
func notify(title, body string) tea.Cmd {
	clean := func(s string) string { return strings.NewReplacer(";", ",", "\x07", "", "\x1b", "").Replace(s) }
	return tea.Raw("\x1b]777;notify;" + clean(title) + ";" + clean(body) + "\x07")
}

// shownSet is the sessions on screen now.
func (d *deskTab) shownSet() map[string]bool {
	out := map[string]bool{}
	for n := range d.termBoxes {
		out[n] = true
	}
	return out
}

// ---- layout ----

// frame is the screen's parts: the sidebar, the head line, the panes'
// area and the foot line.
func (d *deskTab) frame() (side box, head int, area box, foot int) {
	w, h := d.w, d.h
	sw := 0
	if d.sidebar && w >= 60 {
		sw = min(max(w*22/100, 28), 40)
	}
	side = box{0, 0, sw, h}
	x := 0
	if sw > 0 {
		x = sw + 1
	}
	area = box{x, 1, max(0, w-x), max(0, h-2)}
	return side, 0, area, h - 1
}

// panes is the shown workspace's sessions and where each goes; bar: one
// shows at a time, under a bar of tabs (its first line).
func (d *deskTab) panes() ([]fleetapi.DeskSession, []box, bool) {
	ss := d.in(d.active)
	w, _ := d.ws(d.active)
	_, _, area, _ := d.frame()
	fi := slices.IndexFunc(ss, func(s fleetapi.DeskSession) bool { return s.Name == d.focused() })
	bs := paneBoxes(w.Layout, len(ss), fi, area, d.zoom)
	if split(bs) || len(ss) == 0 {
		return ss, bs, false
	}
	for i := range bs {
		if bs[i].w > 0 {
			bs[i].y, bs[i].h = bs[i].y+1, max(0, bs[i].h-1)
		}
	}
	return ss, bs, true
}

// termBox is a pane's terminal: under its head when panes are split.
func termBox(b box, splitView bool) box {
	if splitView {
		return box{b.x, b.y + 1, b.w, max(0, b.h-1)}
	}
	return b
}

// arrange sizes the shown terminals to their panes and tells the keeper
// which are shown.
func (d *deskTab) arrange(c *Ctx) {
	if d.mgr == nil || d.list == nil || d.w == 0 {
		return
	}
	ss, bs, _ := d.panes()
	sp := split(bs)
	d.termBoxes = map[string]box{}
	var shown []string
	for i, s := range ss {
		if bs[i].w == 0 || !s.Running {
			continue
		}
		tb := termBox(bs[i], sp)
		if tb.w < 2 || tb.h < 1 {
			continue
		}
		t := d.mgr.get(s.Name, tb.w, tb.h)
		d.mgr.resize(t, tb.w, tb.h)
		d.termBoxes[s.Name] = tb
		shown = append(shown, s.Name)
		delete(d.unread, s.Name)
	}
	onDesk := map[string]bool{}
	for _, s := range d.sessions() {
		onDesk[s.Name] = true
	}
	d.mgr.keep(shown, d.prewarm(), onDesk, c.Now)
}

// prewarm is each other workspace's focused session, made at the size its
// pane would have: connected before it is first shown, so that going to
// any workspace shows it at once.
func (d *deskTab) prewarm() []string {
	_, _, area, _ := d.frame()
	var out []string
	for _, w := range d.workspaces() {
		if w.ID == d.active {
			continue
		}
		ss := d.in(w.ID)
		fi := slices.IndexFunc(ss, func(s fleetapi.DeskSession) bool { return s.Name == d.focus[w.ID] })
		if fi < 0 {
			fi = 0
		}
		if len(ss) == 0 || !ss[fi].Running {
			continue
		}
		bs := paneBoxes(w.Layout, len(ss), fi, area, false)
		tb := termBox(bs[fi], split(bs))
		if !split(bs) {
			tb.y, tb.h = tb.y+1, tb.h-1 // under its tab bar
		}
		if tb.w < 2 || tb.h < 1 {
			continue
		}
		t := d.mgr.get(ss[fi].Name, tb.w, tb.h)
		if t.emu.Width() == 0 || t.emu.Height() == 0 {
			continue
		}
		out = append(out, ss[fi].Name)
	}
	return out
}

// ---- keys ----

func (d *deskTab) key(m tea.KeyPressMsg, c *Ctx) tea.Cmd {
	k := m.String()
	switch {
	case d.confirm != nil:
		cf := d.confirm
		d.confirm = nil
		return cf.update(k)
	case d.form != nil:
		return d.formKey(m, c)
	case d.ask != nil:
		return d.ask.key(d, m, c)
	case d.pick != nil:
		return d.pick.key(d, k, c)
	case d.panel != nil:
		return d.panel.key(d, k)
	}
	if cmd, ok := d.superKey(k, c); ok {
		return cmd
	}
	if d.mode == typeMode {
		return d.typeKey(m, c)
	}
	return d.navKey(k, c)
}

// superKey is ⌘ with a key, when the terminal passes ⌘ on (kitty's
// keyboard protocol): the same in both modes.
func (d *deskTab) superKey(k string, c *Ctx) (tea.Cmd, bool) {
	rest, ok := strings.CutPrefix(k, "super+")
	if !ok {
		return nil, false
	}
	switch rest {
	case "b":
		return d.toggleSidebar(), true
	case "1", "2", "3", "4", "5", "6", "7", "8", "9":
		return d.toWorkspace(int(rest[0] - '1')), true
	case "up", "alt+up":
		if !d.paneStep(0, -1) {
			d.stepWorkspace(-1)
		}
		return nil, true
	case "down", "alt+down":
		if !d.paneStep(0, 1) {
			d.stepWorkspace(1)
		}
		return nil, true
	case "left", "alt+left":
		if !d.paneStep(-1, 0) {
			d.stepSession(-1)
		}
		return nil, true
	case "right", "alt+right":
		if !d.paneStep(1, 0) {
			d.stepSession(1)
		}
		return nil, true
	case "[", "shift+[", "{":
		d.stepSession(-1)
		return nil, true
	case "]", "shift+]", "}":
		d.stepSession(1)
		return nil, true
	case "n", "t", "e":
		return d.openForm(c), true
	case "shift+n", "shift+t", "shift+e":
		d.askWorkspace()
		return nil, true
	case "enter":
		d.zoom = !d.zoom
		return nil, true
	case "\\":
		return d.cycleLayout(c), true
	case "k", "j":
		d.mode = navMode
		d.syncCursor()
		return nil, true
	case "u", "shift+u":
		return d.jump(), true
	}
	return nil, false
}

// typeKey: a key for the focused session, but Ctrl-] (step out) and
// Ctrl-V (paste a file or an image).
func (d *deskTab) typeKey(m tea.KeyPressMsg, c *Ctx) tea.Cmd {
	k := m.String()
	switch k {
	case "ctrl+]":
		d.mode = navMode
		d.syncCursor()
		return nil
	}
	name := d.focused()
	t := d.term(name)
	if t == nil {
		d.mode = navMode
		return nil
	}
	st, _, _ := t.status()
	switch st {
	case termLive:
		if k == "ctrl+v" {
			return tea.Batch(ctrlV(c.Client, c.Clipboard, name, c.Local), d.uploading(c, "the clipboard"))
		}
		t.key(m)
	case termHeld, termReleased:
		if k == "enter" {
			d.mgr.takeOver(t)
		}
	case termDown, termIdle:
		if k == "enter" {
			d.mgr.retryNow(t)
		}
	}
	return nil
}

// uploading says a paste is on its way, when it takes a moment.
func (d *deskTab) uploading(c *Ctx, what string) tea.Cmd {
	if c.Clipboard == nil && what == "the clipboard" {
		return nil
	}
	return toastCmd("↑ sending "+what+"…", false)
}

func (d *deskTab) term(name string) *deskTerm {
	if d.mgr == nil || name == "" {
		return nil
	}
	return d.mgr.peek(name)
}

// navKey: the keys that move around the desk and change it.
func (d *deskTab) navKey(k string, c *Ctx) tea.Cmd {
	rows := d.rows()
	switch k {
	case "up", "k", "down", "j":
		by := 1
		if k == "up" || k == "k" {
			by = -1
		}
		d.cursor = min(max(d.cursor+by, 0), max(len(rows)-1, 0))
		d.follow()
		return nil
	case "enter", "i", "esc", "right":
		if d.cursor < len(rows) && rows[d.cursor].session != "" {
			d.focus[d.active] = rows[d.cursor].session
		}
		if d.focused() != "" {
			d.mode = typeMode
			d.saveUI()
		}
		return nil
	case "left", "h":
		for i := d.cursor; i >= 0 && i < len(rows); i-- {
			if rows[i].session == "" {
				d.cursor = i
				break
			}
		}
		return nil
	case "b":
		return d.toggleSidebar()
	case "n":
		return d.openForm(c)
	case "W", "N":
		d.askWorkspace()
		return nil
	case "l":
		return d.cycleLayout(c)
	case "z":
		d.zoom = !d.zoom
		return nil
	case "[":
		d.stepSession(-1)
		return nil
	case "]":
		d.stepSession(1)
		return nil
	case "{":
		d.stepWorkspace(-1)
		return nil
	case "}":
		d.stepWorkspace(1)
		return nil
	case "r":
		d.askRename()
		return nil
	case "m":
		if s := d.cursorSession(); s != "" {
			d.pick = newWsPick(d, s)
		}
		return nil
	case "x", "delete":
		return d.askEnd(c)
	case "u":
		return d.jump()
	case "!":
		d.panel = &noticePanel{}
		return nil
	case "K", "shift+up":
		return d.reorder(c, -1)
	case "J", "shift+down":
		return d.reorder(c, 1)
	}
	if n, ok := strings.CutPrefix(k, "alt+"); ok && len(n) == 1 && n[0] >= '1' && n[0] <= '9' {
		return d.toWorkspace(int(n[0] - '1'))
	}
	return nil
}

// follow makes what the cursor is on the shown: its workspace, its session.
func (d *deskTab) follow() {
	rows := d.rows()
	if d.cursor >= len(rows) {
		return
	}
	r := rows[d.cursor]
	if r.ws != d.active {
		d.active = r.ws
		d.zoom = false
		// the rows changed under the cursor: keep it on this workspace
		for i, x := range d.rows() {
			if x.ws == r.ws && x.session == "" {
				d.cursor = i
			}
		}
	}
	if r.session != "" {
		d.focus[d.active] = r.session
	}
	d.saveUI()
}

// cursorSession is the session the cursor is on, else the focused one.
func (d *deskTab) cursorSession() string {
	rows := d.rows()
	if d.cursor < len(rows) && rows[d.cursor].session != "" {
		return rows[d.cursor].session
	}
	return d.focused()
}

func (d *deskTab) toggleSidebar() tea.Cmd {
	d.sidebar = !d.sidebar
	d.saveUI()
	return nil
}

func (d *deskTab) toWorkspace(i int) tea.Cmd {
	wss := d.workspaces()
	if i < 0 || i >= len(wss) {
		return nil
	}
	d.active, d.zoom = wss[i].ID, false
	if d.mode == navMode {
		d.syncCursor()
	}
	d.saveUI()
	return nil
}

func (d *deskTab) stepWorkspace(by int) {
	wss := d.workspaces()
	i := slices.IndexFunc(wss, func(w fleetapi.DeskWorkspace) bool { return w.ID == d.active })
	if len(wss) == 0 {
		return
	}
	d.toWorkspace((i + by + len(wss)) % len(wss))
}

func (d *deskTab) stepSession(by int) {
	ss := d.in(d.active)
	if len(ss) == 0 {
		return
	}
	i := slices.IndexFunc(ss, func(s fleetapi.DeskSession) bool { return s.Name == d.focused() })
	d.focus[d.active] = ss[(i+by+len(ss))%len(ss)].Name
	if d.mode == navMode {
		d.syncCursor()
	}
	d.saveUI()
}

// paneStep moves the focus to the pane beside it; false when there is
// none that way.
func (d *deskTab) paneStep(dx, dy int) bool {
	ss, bs, _ := d.panes()
	if !split(bs) {
		return false
	}
	fi := slices.IndexFunc(ss, func(s fleetapi.DeskSession) bool { return s.Name == d.focused() })
	j := neighbour(bs, fi, dx, dy)
	if j < 0 {
		return false
	}
	d.focus[d.active] = ss[j].Name
	d.saveUI()
	return true
}

// ---- changes to the desk ----

// do sends a change to fleetd: the poll that follows shows it.
func (d *deskTab) do(c *Ctx, say string, f func(ctx context.Context, cl fleetapi.Client) (deskDoneMsg, error)) tea.Cmd {
	cl := c.Client
	if cl == nil {
		return nil
	}
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
		defer cancel()
		m, err := f(ctx, cl)
		if err != nil {
			return deskDoneMsg{err: err}
		}
		if m.say == "" {
			m.say = say
		}
		return m
	}
}

func (d *deskTab) cycleLayout(c *Ctx) tea.Cmd {
	w, ok := d.ws(d.active)
	if !ok {
		return nil
	}
	next := nextLayout(w.Layout)
	d.setLayout(w.ID, next) // at once, on screen; fleetd keeps it
	d.zoom = false
	return d.do(c, "layout: "+next, func(ctx context.Context, cl fleetapi.Client) (deskDoneMsg, error) {
		return deskDoneMsg{}, cl.DeskWorkspaceUpdate(ctx, w.ID, fleetapi.DeskWorkspaceUpdate{Layout: &next})
	})
}

func (d *deskTab) setLayout(id, layout string) {
	for i := range d.list.Workspaces {
		if d.list.Workspaces[i].ID == id {
			d.list.Workspaces[i].Layout = layout
		}
	}
}

func (d *deskTab) openForm(c *Ctx) tea.Cmd {
	d.form = newLaunchForm(c.Theme, d.recents())
	if s, ok := d.session(d.focused()); ok && s.Cwd != "" {
		d.form.dir.SetValue(shortDir(s.Cwd) + "/")
		d.form.dir.CursorEnd()
		return d.form.edited()
	}
	if len(d.form.recents) == 0 {
		return d.form.lookup(c)
	}
	return nil
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
		if !slices.Contains(out, x) {
			out = append(out, x)
		}
	}
	return out
}

// made is a launch's answer: the new session is focused, and typed in.
func (d *deskTab) made(m deskMadeMsg) tea.Cmd {
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
	if d.list != nil && !slices.ContainsFunc(d.list.Sessions, func(x fleetapi.DeskSession) bool { return x.Name == s.Name }) {
		if s.Workspace == "" {
			s.Workspace = d.active
		}
		d.list.Sessions = append(d.list.Sessions, s)
	}
	d.active, d.focus[s.Workspace] = s.Workspace, s.Name
	d.mode = typeMode
	d.saveUI()
	return toastCmd("launched "+s.Name+" · Ctrl-] steps out", false)
}

func (d *deskTab) askWorkspace() {
	d.ask = newAskLine("New workspace", "a name", "", func(name string, c *Ctx) tea.Cmd {
		return d.do(c, "", func(ctx context.Context, cl fleetapi.Client) (deskDoneMsg, error) {
			w, err := cl.DeskWorkspaceCreate(ctx, fleetapi.DeskWorkspaceCreate{Name: name})
			if err != nil {
				return deskDoneMsg{}, err
			}
			return deskDoneMsg{say: "made " + w.Name + " · n starts a session in it", active: w.ID}, nil
		})
	})
}

func (d *deskTab) askRename() {
	rows := d.rows()
	if d.cursor < len(rows) && rows[d.cursor].session == "" && d.mode == navMode {
		w, ok := d.ws(rows[d.cursor].ws)
		if !ok {
			return
		}
		d.ask = newAskLine("Rename "+w.Name, "the workspace's name", w.Name, func(name string, c *Ctx) tea.Cmd {
			return d.do(c, "renamed to "+name, func(ctx context.Context, cl fleetapi.Client) (deskDoneMsg, error) {
				return deskDoneMsg{}, cl.DeskWorkspaceUpdate(ctx, w.ID, fleetapi.DeskWorkspaceUpdate{Name: &name})
			})
		})
		return
	}
	s, ok := d.session(d.cursorSession())
	if !ok {
		return
	}
	d.ask = newAskLine("Rename "+shownName(s), "a name to show (empty: "+s.Name+"'s own)", s.Label, func(label string, c *Ctx) tea.Cmd {
		return d.do(c, "renamed", func(ctx context.Context, cl fleetapi.Client) (deskDoneMsg, error) {
			return deskDoneMsg{}, cl.DeskUpdate(ctx, s.Name, fleetapi.DeskUpdate{Label: &label})
		})
	})
	d.ask.empty = true
}

func (d *deskTab) askEnd(c *Ctx) tea.Cmd {
	rows := d.rows()
	if d.cursor < len(rows) && rows[d.cursor].session == "" {
		w, ok := d.ws(rows[d.cursor].ws)
		if !ok {
			return nil
		}
		n := len(d.in(w.ID))
		if len(d.workspaces()) == 1 {
			return toastCmd("the desk keeps one workspace at least", true)
		}
		ask := "Remove " + w.Name + "?"
		if n > 0 {
			ask = fmt.Sprintf("Remove %s and end its %d session(s)?", w.Name, n)
		}
		d.confirm = &confirm{ask: ask, act: d.do(c, "removed "+w.Name, func(ctx context.Context, cl fleetapi.Client) (deskDoneMsg, error) {
			return deskDoneMsg{}, cl.DeskWorkspaceDelete(ctx, w.ID, n > 0)
		})}
		return nil
	}
	s, ok := d.session(d.cursorSession())
	if !ok {
		return nil
	}
	why := "Its Claude Code session stays on disk."
	if !isClaude(s.Command) {
		why = "What runs in it stops."
	}
	d.confirm = &confirm{ask: fmt.Sprintf("End %s? %s", shownName(s), why), act: d.do(c, "ended "+shownName(s), func(ctx context.Context, cl fleetapi.Client) (deskDoneMsg, error) {
		return deskDoneMsg{}, cl.DeskEnd(ctx, s.Name)
	})}
	return nil
}

// reorder moves what the cursor is on, a session in its workspace or a
// workspace among them, by one.
func (d *deskTab) reorder(c *Ctx, by int) tea.Cmd {
	rows := d.rows()
	if d.cursor >= len(rows) {
		return nil
	}
	r := rows[d.cursor]
	if r.session == "" {
		wss := d.workspaces()
		i := slices.IndexFunc(wss, func(w fleetapi.DeskWorkspace) bool { return w.ID == r.ws })
		j := i + by
		if i < 0 || j < 0 || j >= len(wss) {
			return nil
		}
		before := ""
		if by < 0 {
			before = wss[j].ID
		} else if j+1 < len(wss) {
			before = wss[j+1].ID
		}
		wss[i], wss[j] = wss[j], wss[i] // at once, on screen
		d.syncCursor()
		return d.do(c, "", func(ctx context.Context, cl fleetapi.Client) (deskDoneMsg, error) {
			return deskDoneMsg{}, cl.DeskWorkspaceUpdate(ctx, r.ws, fleetapi.DeskWorkspaceUpdate{Before: &before})
		})
	}
	ss := d.in(r.ws)
	i := slices.IndexFunc(ss, func(s fleetapi.DeskSession) bool { return s.Name == r.session })
	j := i + by
	if i < 0 || j < 0 || j >= len(ss) {
		return nil
	}
	before := ""
	if by < 0 {
		before = ss[j].Name
	} else if j+1 < len(ss) {
		before = ss[j+1].Name
	}
	a := slices.IndexFunc(d.list.Sessions, func(s fleetapi.DeskSession) bool { return s.Name == ss[i].Name })
	b := slices.IndexFunc(d.list.Sessions, func(s fleetapi.DeskSession) bool { return s.Name == ss[j].Name })
	d.list.Sessions[a], d.list.Sessions[b] = d.list.Sessions[b], d.list.Sessions[a]
	d.cursor += by
	return d.do(c, "", func(ctx context.Context, cl fleetapi.Client) (deskDoneMsg, error) {
		return deskDoneMsg{}, cl.DeskUpdate(ctx, r.session, fleetapi.DeskUpdate{Before: &before})
	})
}

// ---- pastes ----

// paste is text the terminal pasted: a file or folder dropped on it, or
// copied in a file manager, arrives as its path here; it is sent to the
// session's machine, and its path there typed. Any other text is typed.
func (d *deskTab) paste(m tea.PasteMsg, c *Ctx) tea.Cmd {
	switch {
	case d.form != nil:
		return d.form.typeInto(m)
	case d.ask != nil:
		var cmd tea.Cmd
		d.ask.in, cmd = d.ask.in.Update(m)
		return cmd
	case d.mode != typeMode:
		return nil
	}
	name := d.focused()
	t := d.term(name)
	if t == nil {
		return nil
	}
	if files := localPaths(m.Content); len(files) > 0 && !c.Local {
		names := make([]string, len(files))
		for i, f := range files {
			names[i] = filepath.Base(f)
		}
		return tea.Batch(pasteFiles(c.Client, name, files, c.Local), d.uploading(c, strings.Join(names, ", ")))
	}
	t.paste(m.Content)
	return nil
}

// pasted is a paste sent: its paths typed into its session.
func (d *deskTab) pasted(m deskPastedMsg) tea.Cmd {
	if m.err != nil {
		return toastCmd("paste: "+m.err.Error(), true)
	}
	t := d.term(m.session)
	if t == nil {
		return toastCmd(m.session+" is gone: nothing pasted", true)
	}
	if m.kind == "none" { // Ctrl+V with no file or image: the key, as typed
		t.emu.SendText("\x16")
		return nil
	}
	t.paste(typedPaths(m.paths))
	what := strings.Join(m.names, ", ")
	if len(m.names) > 2 {
		what = fmt.Sprintf("%d files", len(m.names))
	}
	if m.bytes > 0 {
		what += " · " + bytesOf(m.bytes)
	}
	return toastCmd("pasted "+what, false)
}

// ---- the mouse ----

func (d *deskTab) click(m tea.Mouse, c *Ctx) tea.Cmd {
	for _, h := range d.hits {
		if !h.b.in(m.X, m.Y) {
			continue
		}
		switch {
		case h.layout:
			return d.cycleLayout(c)
		case h.session != "":
			if s, ok := d.session(h.session); ok {
				d.active, d.focus[s.Workspace] = s.Workspace, s.Name
				d.mode = typeMode
				d.saveUI()
			}
			if h.pane && h.term.in(m.X, m.Y) {
				d.drag = h.session
				d.mouseTo(h.session, uv.MouseClickEvent(toUV(m)))
			}
			return nil
		case h.ws != "":
			d.active, d.zoom = h.ws, false
			d.mode = navMode
			d.syncCursor()
			d.saveUI()
			return nil
		}
	}
	return nil
}

// ---- small questions ----

// askLine is a one-line question in a card: a name.
type askLine struct {
	title string
	in    textinput.Model
	empty bool // an empty answer is one
	send  func(text string, c *Ctx) tea.Cmd
}

func newAskLine(title, placeholder, value string, send func(string, *Ctx) tea.Cmd) *askLine {
	in := textinput.New()
	in.Prompt, in.Placeholder = "", placeholder
	in.SetValue(value)
	in.CursorEnd()
	in.Focus()
	return &askLine{title: title, in: in, send: send}
}

func (a *askLine) key(d *deskTab, m tea.KeyPressMsg, c *Ctx) tea.Cmd {
	switch m.String() {
	case "esc":
		d.ask = nil
		return nil
	case "enter":
		v := strings.TrimSpace(a.in.Value())
		if v == "" && !a.empty {
			return nil
		}
		d.ask = nil
		return a.send(v, c)
	}
	var cmd tea.Cmd
	a.in, cmd = a.in.Update(m)
	return cmd
}

func (a *askLine) view(th *Theme, w int) []string {
	a.in.SetWidth(max(8, w-8))
	lines := []string{""}
	lines = append(lines, wrapped(a.title, w-6, th.Strong)...)
	lines = append(lines, "", a.in.View(), "", hintsFit(th, w-6, "⏎ done", "esc cancel"))
	return deskCard(th.Amber, "", "", lines, w)
}

// wsPick chooses the workspace a session moves to.
type wsPick struct {
	session string
	hi      int
}

func newWsPick(d *deskTab, session string) *wsPick {
	return &wsPick{session: session}
}

func (p *wsPick) key(d *deskTab, k string, c *Ctx) tea.Cmd {
	wss := d.workspaces()
	switch k {
	case "esc", "q":
		d.pick = nil
	case "up", "k":
		p.hi = max(0, p.hi-1)
	case "down", "j":
		p.hi = min(len(wss)-1, p.hi+1)
	case "enter":
		d.pick = nil
		if p.hi < len(wss) {
			to := wss[p.hi]
			name := p.session
			return d.do(c, "moved to "+to.Name, func(ctx context.Context, cl fleetapi.Client) (deskDoneMsg, error) {
				return deskDoneMsg{focus: name}, cl.DeskUpdate(ctx, name, fleetapi.DeskUpdate{Workspace: &to.ID})
			})
		}
	}
	return nil
}

func (p *wsPick) view(d *deskTab, th *Theme, w int) []string {
	s, _ := d.session(p.session)
	lines := []string{""}
	lines = append(lines, wrapped("Move "+shownName(s)+" to", w-6, th.Strong)...)
	lines = append(lines, "")
	for i, ws := range d.workspaces() {
		fg, mark := th.Text, "  "
		if i == p.hi {
			fg, mark = th.Amber, "▸ "
		}
		here := ""
		if ws.ID == s.Workspace {
			here = "  (it is here)"
		}
		lines = append(lines, line(w-6, nil, seg{s: mark, fg: th.Amber}, seg{s: ws.Name, fg: fg, bold: i == p.hi}, seg{s: here, fg: th.Dim}))
	}
	lines = append(lines, "", hintsFit(th, w-6, "⏎ move", "↑↓ choose", "esc cancel"))
	return deskCard(th.Amber, "", "", lines, w)
}
