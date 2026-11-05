package tui

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"image"
	"io"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"time"
	"unicode"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	uv "github.com/charmbracelet/ultraviolet"

	"github.com/MelloB1989/karmax/fleet/internal/fleetapi"
	"github.com/MelloB1989/karmax/fleet/internal/hostfiles"
)

// The Files tab: fleetd's machine's files, and this machine's beside them.
// A folder is listed from a cache at once and asked for again behind it;
// the folder under the cursor is listed before it is opened, and the file
// under it previewed, once the cursor rests. Files move either way as
// transfers (files_xfer.go), over fleetd's local network when this machine
// is on it; a file opens full screen (files_viewer.go): code in its
// colors, markdown as a page, a picture as a picture.

// Sides of the tab.
const (
	sideHost = 0 // fleetd's machine
	sideHere = 1 // this one
)

// Sort orders.
const (
	sortName = "name"
	sortMod  = "modified"
	sortSize = "size"
)

// How the tab waits and keeps.
const (
	pollEvery    = 10 * time.Second // the shown folder, asked again
	pollMax      = 3000             // entries: a bigger folder is not polled
	keepPreviews = 64
	previewRead  = 48 << 10
)

// How long the cursor rests before the preview and the listing ahead, and
// how often the transfers' progress moves (tests make them short).
var (
	restFor  = 140 * time.Millisecond
	xferTick = 300 * time.Millisecond
)

type fpane struct {
	side    int
	path    string
	dir     *fleetapi.FileDir
	rows    []fleetapi.FileEntry // as shown: hidden ones out, filtered, sorted
	cur     int
	top     int
	marks   map[string]bool
	filter  string
	err     string
	loading bool
	back    map[string]string // a folder's cursor when it was left
	want    string            // the entry to put the cursor on once listed
}

func (p *fpane) entry() (fleetapi.FileEntry, bool) {
	if p.cur >= 0 && p.cur < len(p.rows) {
		return p.rows[p.cur], true
	}
	return fleetapi.FileEntry{}, false
}

// join is a path in the pane's folder.
func (p *fpane) join(name string) string {
	if p.side == sideHost {
		return path.Join(p.path, name)
	}
	return filepath.Join(p.path, name)
}

func (p *fpane) parent() string {
	if p.side == sideHost {
		return path.Dir(p.path)
	}
	return filepath.Dir(p.path)
}

// picked is the marked entries, or the one under the cursor.
func (p *fpane) picked() []fleetapi.FileEntry {
	var out []fleetapi.FileEntry
	for _, e := range p.rows {
		if p.marks[e.Name] {
			out = append(out, e)
		}
	}
	if len(out) == 0 {
		if e, ok := p.entry(); ok {
			out = append(out, e)
		}
	}
	return out
}

type fpreview struct {
	kind  string // dir | text | markdown | image | hex | none | err
	lines []string
	img   image.Image
	text  string // markdown, to lay out at the width shown
	err   string
	mdW   int
}

type filesTab struct {
	gfx      string
	asked    bool // the terminal was asked about pictures
	cw, ch   int
	panes    [2]*fpane
	dual     bool
	side     int
	preview  bool
	hidden   bool
	sortBy   string
	downDir  string
	lists    map[string]*fleetapi.FileDir // side:path → as last listed
	listAt   map[string]time.Time
	listing  map[string]bool // in flight
	prevs    map[string]*fpreview
	prevKeys []string
	prevWant map[string]bool
	restSeq  int
	view     *fviewer
	ask      *askLine
	confirm  *confirm
	panel    *xferPanel
	xf       *xfers
	ticking  bool
	gal      gallery
	state    string
	loaded   bool
	local    bool
	w, h     int
	fullW    int
	fullH    int
	blocks   map[string][]string // half-block pictures as drawn, by key@size
	peeked   *fpane              // the folder the preview lists, sorted
	peekKey  string
	pollAt   time.Time
	host     string
	home     string // fleetd's
	hereHome string
	here     *hostfiles.Manager
}

// Messages.
type (
	filesListMsg struct {
		side int
		path string
		dir  *fleetapi.FileDir
		err  error
	}
	filesPrevMsg struct {
		key  string
		prev *fpreview
	}
	filesRestMsg struct{ seq int }
	filesPollMsg struct{}
	filesOpMsg   struct {
		side  int
		say   string
		err   error
		focus string
	}
	// filesFetchMsg asks for a file to be fetched and opened here.
	filesFetchMsg struct{ x *xfer }
)

func newFiles() Tab {
	home, _ := os.UserHomeDir()
	cw, ch := cellPx()
	t := &filesTab{gfx: detectGfx(), cw: cw, ch: ch, preview: true, sortBy: sortName, xf: &xfers{},
		lists: map[string]*fleetapi.FileDir{}, listAt: map[string]time.Time{}, listing: map[string]bool{},
		prevs: map[string]*fpreview{}, prevWant: map[string]bool{}, blocks: map[string][]string{},
		hereHome: home, here: &hostfiles.Manager{Home: home}}
	t.panes[sideHost] = &fpane{side: sideHost, path: "~", marks: map[string]bool{}, back: map[string]string{}}
	t.panes[sideHere] = &fpane{side: sideHere, path: home, marks: map[string]bool{}, back: map[string]string{}}
	t.downDir = filepath.Join(home, "Downloads")
	return t
}

func (t *filesTab) Title() string               { return "Files" }
func (t *filesTab) Visible(me fleetapi.Me) bool { return me.Role == fleetapi.RoleAdmin }
func (t *filesTab) Capturing() bool {
	return t.view != nil || t.ask != nil || t.confirm != nil || t.panel != nil || t.filtering()
}
func (t *filesTab) Fullscreen() bool { return t.view != nil }

// Claims: with both sides shown, tab moves between them.
func (t *filesTab) Claims(k string) bool { return t.dual && (k == "tab" || k == "shift+tab") }

// Background: transfers go on, and their news comes, on any tab.
func (t *filesTab) Background(msg tea.Msg) bool {
	switch msg.(type) {
	case xferDoneMsg, xferTickMsg, filesListMsg, filesPrevMsg, filesOpMsg, uv.KittyGraphicsEvent:
		return true
	}
	return false
}

func (t *filesTab) filtering() bool {
	p := t.pane()
	return p.filter != "" && strings.HasSuffix(p.filter, "\x00")
}

func (t *filesTab) pane() *fpane { return t.panes[t.side] }

func (t *filesTab) Help() []key.Binding {
	if t.dual {
		return []key.Binding{keyIs("tab", "other side"), keyIs("c", "copy across"), keyIs("⏎", "open"), keyIs("space", "mark"),
			keyIs("L", "one side"), keyIs("T", "transfers"), keyIs("n r x", "new, rename, trash"), keyIs("/", "filter"), keyIs(".", "hidden")}
	}
	return []key.Binding{keyIs("⏎", "open"), keyIs("←", "up"), keyIs("d", "download"), keyIs("u", "upload"), keyIs("o", "open here"),
		keyIs("space", "mark"), keyIs("L", "this machine too"), keyIs("T", "transfers"), keyIs("n r x", "new, rename, trash"),
		keyIs("/", "filter"), keyIs("g", "go to"), keyIs(".", "hidden"), keyIs("s", "sort"), keyIs("p", "preview"), keyIs("y", "copy path")}
}

// ---- state kept between runs ----

type filesUI struct {
	Host    string `json:"host_path"`
	Here    string `json:"here_path"`
	Hidden  bool   `json:"hidden"`
	Sort    string `json:"sort"`
	Preview *bool  `json:"preview"`
	Down    string `json:"downloads"`
	Dual    bool   `json:"dual"`
}

func (t *filesTab) loadUI(c *Ctx) {
	t.loaded, t.local, t.state = true, c.Local, c.FilesState
	if t.state == "" {
		return
	}
	b, err := os.ReadFile(t.state)
	if err != nil {
		return
	}
	var s filesUI
	if json.Unmarshal(b, &s) != nil {
		return
	}
	if s.Host != "" {
		t.panes[sideHost].path = s.Host
	}
	if s.Here != "" {
		if fi, err := os.Stat(s.Here); err == nil && fi.IsDir() {
			t.panes[sideHere].path = s.Here
		}
	}
	t.hidden, t.dual = s.Hidden, s.Dual
	if s.Sort != "" {
		t.sortBy = s.Sort
	}
	if s.Preview != nil {
		t.preview = *s.Preview
	}
	if s.Down != "" {
		t.downDir = s.Down
	}
}

func (t *filesTab) saveUI() {
	if t.state == "" {
		return
	}
	pv := t.preview
	b, _ := json.MarshalIndent(filesUI{Host: t.panes[sideHost].path, Here: t.panes[sideHere].path, Hidden: t.hidden,
		Sort: t.sortBy, Preview: &pv, Down: t.downDir, Dual: t.dual}, "", "  ")
	_ = os.MkdirAll(filepath.Dir(t.state), 0o700)
	_ = os.WriteFile(t.state, append(b, '\n'), 0o600)
}

// ---- listing ----

func listKey(side int, p string) string { return fmt.Sprintf("%d:%s", side, p) }

// list asks for a folder; quiet: no "loading" while it comes.
func (t *filesTab) list(side int, p string, c *Ctx) tea.Cmd {
	k := listKey(side, p)
	if t.listing[k] {
		return nil
	}
	t.listing[k] = true
	cl, here := c.Client, t.here
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if side == sideHere {
			d, err := here.List(p)
			if err != nil {
				return filesListMsg{side: side, path: p, err: err}
			}
			return filesListMsg{side: side, path: p, dir: &d}
		}
		d, err := cl.FilesList(ctx, p)
		return filesListMsg{side: side, path: p, dir: d, err: err}
	}
}

// goTo shows folder p in a pane: from the cache at once when it is there,
// asked for again either way.
func (t *filesTab) goTo(pn *fpane, p string, c *Ctx) tea.Cmd {
	if pn.dir != nil && pn.path != "" {
		if e, ok := pn.entry(); ok {
			pn.back[pn.dir.Path] = e.Name
		}
	}
	pn.path, pn.err, pn.cur, pn.top, pn.marks = p, "", 0, 0, map[string]bool{}
	if f := pn.filter; f != "" {
		pn.filter = ""
	}
	if d := t.lists[listKey(pn.side, p)]; d != nil {
		t.apply(pn, d)
	} else {
		pn.loading, pn.dir, pn.rows = true, nil, nil
	}
	t.saveUI()
	return tea.Batch(t.list(pn.side, p, c), t.rest())
}

// apply shows a listing in a pane, the cursor where it was.
func (t *filesTab) apply(pn *fpane, d *fleetapi.FileDir) {
	name := pn.want
	if name == "" {
		if e, ok := pn.entry(); ok && pn.dir != nil && pn.dir.Path == d.Path {
			name = e.Name
		} else if d.Focus != "" {
			name = d.Focus
		} else {
			name = pn.back[d.Path]
		}
	}
	pn.dir, pn.path, pn.loading, pn.want = d, d.Path, false, ""
	if pn.side == sideHost {
		t.host, t.home = cmp.Or(d.Host, t.host), cmp.Or(d.Home, t.home)
	}
	t.relist(pn)
	if name != "" {
		for i, e := range pn.rows {
			if e.Name == name {
				pn.cur = i
				break
			}
		}
	}
	pn.cur = min(max(pn.cur, 0), max(0, len(pn.rows)-1))
}

// relist works out a pane's rows: hidden ones, the filter, the order.
func (t *filesTab) relist(pn *fpane) {
	pn.rows = pn.rows[:0]
	if pn.dir == nil {
		return
	}
	f := strings.ToLower(strings.TrimSuffix(pn.filter, "\x00"))
	for _, e := range pn.dir.Entries {
		if !t.hidden && strings.HasPrefix(e.Name, ".") && f == "" {
			continue
		}
		if f != "" && !strings.Contains(strings.ToLower(e.Name), f) {
			continue
		}
		pn.rows = append(pn.rows, e)
	}
	by := t.sortBy
	slices.SortStableFunc(pn.rows, func(a, b fleetapi.FileEntry) int {
		if a.IsDir() != b.IsDir() {
			if a.IsDir() {
				return -1
			}
			return 1
		}
		switch by {
		case sortMod:
			if c := b.Mod.Compare(a.Mod); c != 0 {
				return c
			}
		case sortSize:
			if c := cmp.Compare(b.Size, a.Size); c != 0 {
				return c
			}
		}
		return natural(a.Name, b.Name)
	})
}

// natural orders names as people do: case aside, file2 before file10.
func natural(a, b string) int {
	ar, br := []rune(strings.ToLower(a)), []rune(strings.ToLower(b))
	i, j := 0, 0
	for i < len(ar) && j < len(br) {
		if unicode.IsDigit(ar[i]) && unicode.IsDigit(br[j]) {
			si, sj := i, j
			for i < len(ar) && unicode.IsDigit(ar[i]) {
				i++
			}
			for j < len(br) && unicode.IsDigit(br[j]) {
				j++
			}
			na := strings.TrimLeft(string(ar[si:i]), "0")
			nb := strings.TrimLeft(string(br[sj:j]), "0")
			if c := cmp.Compare(len(na), len(nb)); c != 0 {
				return c
			}
			if c := strings.Compare(na, nb); c != 0 {
				return c
			}
			continue
		}
		if c := cmp.Compare(ar[i], br[j]); c != 0 {
			return c
		}
		i, j = i+1, j+1
	}
	if c := cmp.Compare(len(ar)-i, len(br)-j); c != 0 {
		return c
	}
	return strings.Compare(a, b)
}

// ---- the cursor rests: list ahead, preview ----

func (t *filesTab) rest() tea.Cmd {
	t.restSeq++
	seq := t.restSeq
	return tea.Tick(restFor, func(time.Time) tea.Msg { return filesRestMsg{seq} })
}

func prevKey(side int, full string, e fleetapi.FileEntry) string {
	return fmt.Sprintf("%d:%s:%d:%d", side, full, e.Size, e.Mod.UnixNano())
}

// rested: list the folder under the cursor ahead of time, or preview the
// file.
func (t *filesTab) rested(c *Ctx) tea.Cmd {
	pn := t.pane()
	e, ok := pn.entry()
	if !ok {
		return nil
	}
	full := pn.join(e.Name)
	if e.IsDir() {
		k := listKey(pn.side, full)
		if d := t.lists[k]; d != nil && time.Since(t.listAt[k]) < 5*time.Second {
			return nil
		}
		return t.list(pn.side, full, c)
	}
	if !t.preview && t.view == nil {
		return nil
	}
	return t.fetchPreview(pn.side, full, e, c)
}

func (t *filesTab) fetchPreview(side int, full string, e fleetapi.FileEntry, c *Ctx) tea.Cmd {
	k := prevKey(side, full, e)
	if t.prevs[k] != nil || t.prevWant[k] {
		return nil
	}
	t.prevWant[k] = true
	cl, here, dark := c.Client, t.here, c.Theme.Dark
	pw, ph := t.previewPx()
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		defer cancel()
		return filesPrevMsg{key: k, prev: makePreview(ctx, cl, here, side, full, e, pw, ph, dark)}
	}
}

// previewPx is the preview column's size in pixels, for a picture.
func (t *filesTab) previewPx() (int, int) {
	_, prev := t.columns(t.w)
	if prev.w <= 0 {
		prev = box{w: 60, h: 30}
	}
	return max(64, (prev.w-2)*t.cw), max(64, (t.h-4)*t.ch)
}

// makePreview reads what the preview shows of a file.
func makePreview(ctx context.Context, cl fleetapi.Client, here *hostfiles.Manager, side int, full string, e fleetapi.FileEntry, pw, ph int, dark bool) *fpreview {
	name := e.Name
	if pictured(name) {
		var b []byte
		var err error
		if side == sideHost {
			b, err = cl.FilesPreview(ctx, full, pw, ph)
		} else {
			b, _, err = here.Preview(ctx, full, pw, ph)
		}
		if err != nil {
			return &fpreview{kind: "err", err: oneLine(err.Error())}
		}
		img, err := decodePic(b)
		if err != nil {
			return &fpreview{kind: "err", err: "not a picture this can show"}
		}
		return &fpreview{kind: showImage, img: img}
	}
	if e.Kind == fleetapi.KindOther {
		return &fpreview{kind: showNone}
	}
	head, err := readHead(ctx, cl, side, full, previewRead)
	if err != nil {
		return &fpreview{kind: "err", err: oneLine(err.Error())}
	}
	switch as := showAs(name, head); as {
	case showHex:
		return &fpreview{kind: showHex, lines: hexLines(head[:min(len(head), 1024)], 0)}
	case showMarkdown:
		return &fpreview{kind: showMarkdown, text: string(head)}
	default:
		return &fpreview{kind: showText, lines: highlight(name, string(head), dark)}
	}
}

// readHead is the first n bytes of a file on either side.
func readHead(ctx context.Context, cl fleetapi.Client, side int, full string, n int64) ([]byte, error) {
	if side == sideHere {
		f, err := os.Open(full)
		if err != nil {
			return nil, err
		}
		defer f.Close()
		return io.ReadAll(io.LimitReader(f, n))
	}
	r, err := cl.FilesRead(ctx, full, 0, n, time.Time{})
	if err != nil {
		return nil, err
	}
	defer r.Body.Close()
	return io.ReadAll(io.LimitReader(r.Body, n))
}

func (t *filesTab) keepPreview(k string, p *fpreview) {
	delete(t.prevWant, k)
	if _, ok := t.prevs[k]; !ok {
		t.prevKeys = append(t.prevKeys, k)
	}
	t.prevs[k] = p
	for len(t.prevKeys) > keepPreviews {
		delete(t.prevs, t.prevKeys[0])
		t.prevKeys = t.prevKeys[1:]
	}
}

// ---- update ----

func (t *filesTab) Update(msg tea.Msg, c *Ctx) tea.Cmd {
	if !t.loaded {
		t.loadUI(c)
	}
	cmds := []tea.Cmd{t.update(msg, c)}
	if t.panes[sideHost].dir == nil && !t.listing[listKey(sideHost, t.panes[sideHost].path)] && t.panes[sideHost].err == "" {
		t.panes[sideHost].loading = true
		cmds = append(cmds, t.list(sideHost, t.panes[sideHost].path, c))
	}
	if t.dual && t.panes[sideHere].dir == nil && !t.listing[listKey(sideHere, t.panes[sideHere].path)] && t.panes[sideHere].err == "" {
		cmds = append(cmds, t.list(sideHere, t.panes[sideHere].path, c))
	}
	if !t.asked && t.gfx == gfxBlocks && os.Getenv("FLEET_IMAGES") == "" && os.Getenv("TMUX") == "" {
		t.asked = true
		cmds = append(cmds, kittyQuery())
	}
	cmds = append(cmds, t.placePics())
	return tea.Batch(cmds...)
}

func (t *filesTab) update(msg tea.Msg, c *Ctx) tea.Cmd {
	switch m := msg.(type) {
	case tea.WindowSizeMsg:
		t.fullW, t.fullH = m.Width, m.Height
		if t.view != nil {
			t.w, t.h = m.Width, m.Height
		} else {
			t.w, t.h = m.Width-4, m.Height-frameRows
		}
		if w, h := termCellPx(); w > 0 && h > 0 {
			t.cw, t.ch = w, h
		}
		return nil

	case uv.KittyGraphicsEvent:
		if m.Options.ID == kittyQueryID && strings.HasPrefix(string(m.Payload), "OK") && kittyAnswered() {
			t.gfx = gfxKitty
		}
		return nil

	case SnapshotMsg, FrameMsg:
		if t.view == nil && time.Since(t.pollAt) > pollEvery {
			t.pollAt = time.Now()
			pn := t.panes[sideHost]
			if pn.dir != nil && len(pn.dir.Entries) < pollMax {
				return t.list(sideHost, pn.path, c)
			}
		}
		return nil

	case filesListMsg:
		k := listKey(m.side, m.path)
		delete(t.listing, k)
		pn := t.panes[m.side]
		if m.err != nil {
			if pn.path == m.path {
				pn.loading, pn.err = false, oneLine(m.err.Error())
				if pn.dir != nil && pn.dir.Path != m.path {
					pn.dir, pn.rows = nil, nil
				}
			}
			return nil
		}
		t.lists[k], t.listAt[k] = m.dir, time.Now()
		if m.dir.Path != m.path {
			t.lists[listKey(m.side, m.dir.Path)], t.listAt[listKey(m.side, m.dir.Path)] = m.dir, time.Now()
		}
		if pn.path == m.path || pn.path == m.dir.Path {
			first := pn.dir == nil
			t.apply(pn, m.dir)
			if first {
				return t.rest()
			}
		}
		return nil

	case filesRestMsg:
		if m.seq != t.restSeq {
			return nil
		}
		return t.rested(c)

	case filesPrevMsg:
		t.keepPreview(m.key, m.prev)
		return nil

	case filesOpMsg:
		pn := t.panes[m.side]
		if m.focus != "" {
			pn.want = m.focus
		}
		cmd := t.list(m.side, pn.path, c)
		if m.err != nil {
			return tea.Batch(cmd, toast(oneLine(m.err.Error()), true))
		}
		return tea.Batch(cmd, toast(m.say, false))

	case xferDoneMsg:
		cmds := []tea.Cmd{t.xf.pump(c.Client)}
		if x := t.xf.byID(m.id); x != nil {
			cmds = append(cmds, t.xferNews(x, c))
		}
		return tea.Batch(cmds...)

	case xferTickMsg:
		t.xf.tick(time.Now())
		if t.xf.active() == 0 {
			t.ticking = false
			return nil
		}
		return tea.Tick(xferTick, func(time.Time) tea.Msg { return xferTickMsg{} })

	case viewLoadedMsg:
		if t.view != nil {
			return t.view.loaded(m, t, c)
		}
		return nil

	case tea.PasteMsg:
		return t.paste(m, c)

	case filesClipMsg: // Ctrl+V's clipboard, read: what it held goes up
		return t.clipped(m, c)

	case tea.KeyPressMsg:
		return t.key(m, c)
	}
	return nil
}

// xferNews is a transfer's end, said; the folder it went into listed again.
func (t *filesTab) xferNews(x *xfer, c *Ctx) tea.Cmd {
	x.mu.Lock()
	err, out, open := x.err, x.out, x.open
	x.mu.Unlock()
	var cmds []tea.Cmd
	side := sideHere
	if x.up {
		side = sideHost
	}
	if pn := t.panes[side]; pn.dir != nil && (pn.path == x.dst || t.local) {
		if out != "" {
			pn.want = filepath.Base(out)
		}
		cmds = append(cmds, t.list(side, pn.path, c))
	}
	switch s := x.state.Load(); {
	case s == xferCancelled:
		cmds = append(cmds, toast(x.name+": stopped", false))
	case err != nil:
		cmds = append(cmds, toast(x.name+": "+oneLine(err.Error()), true))
	case open:
		cmds = append(cmds, toast("opened "+x.name+" here", false))
	case x.up:
		cmds = append(cmds, toast(fmt.Sprintf("%s is on %s: %s", x.name, cmp.Or(t.host, "fleetd's machine"), t.tildeHost(out)), false))
	default:
		cmds = append(cmds, toast("downloaded "+x.name+" to "+t.tildeHere(out), false))
	}
	return tea.Batch(cmds...)
}

func (t *filesTab) tildeHere(p string) string {
	switch {
	case t.hereHome == "":
	case p == t.hereHome:
		return "~"
	case strings.HasPrefix(p, t.hereHome+string(filepath.Separator)):
		return "~" + p[len(t.hereHome):]
	}
	return p
}

// tildeHost is a path on fleetd's machine with its home as ~.
func (t *filesTab) tildeHost(p string) string {
	return t.shownPath(&fpane{side: sideHost, path: p})
}

// queue adds transfers and starts what there is room for.
func (t *filesTab) queue(c *Ctx, xs ...*xfer) tea.Cmd {
	for _, x := range xs {
		t.xf.add(x)
	}
	cmds := []tea.Cmd{t.xf.pump(c.Client)}
	if !t.ticking {
		t.ticking = true
		cmds = append(cmds, tea.Tick(xferTick, func(time.Time) tea.Msg { return xferTickMsg{} }))
	}
	return tea.Batch(cmds...)
}

// ---- keys ----

func (t *filesTab) key(m tea.KeyPressMsg, c *Ctx) tea.Cmd {
	k := m.String()
	switch {
	case t.view != nil:
		return t.view.key(m, t, c)
	case t.ask != nil:
		done, cmd := t.ask.update(m, c)
		if done {
			t.ask = nil
		}
		return cmd
	case t.confirm != nil:
		cmd := t.confirm.update(k)
		t.confirm = nil
		return cmd
	case t.panel != nil:
		return t.panel.key(t, k, c)
	case t.filtering():
		return t.filterKey(m, c)
	}
	pn := t.pane()
	move := func(to int) tea.Cmd {
		if len(pn.rows) == 0 {
			return nil
		}
		to = min(max(to, 0), len(pn.rows)-1)
		if to == pn.cur {
			return nil
		}
		pn.cur = to
		return t.rest()
	}
	page := max(1, t.h-4)
	switch k {
	case "up", "k":
		return move(pn.cur - 1)
	case "down", "j":
		return move(pn.cur + 1)
	case "pgup", "ctrl+b":
		return move(pn.cur - page)
	case "pgdown", "ctrl+f":
		return move(pn.cur + page)
	case "home":
		return move(0)
	case "end", "G":
		return move(len(pn.rows) - 1)
	case "left", "h", "backspace":
		if pn.dir == nil || pn.parent() == pn.path {
			return nil
		}
		pn.want = filepath.Base(pn.path)
		return t.goTo(pn, pn.parent(), c)
	case "right", "l", "enter":
		e, ok := pn.entry()
		if !ok {
			return nil
		}
		if e.IsDir() {
			return t.goTo(pn, pn.join(e.Name), c)
		}
		return t.openView(pn, e, c)
	case "tab", "shift+tab":
		if t.dual {
			t.side = 1 - t.side
			return t.rest()
		}
	case "esc":
		if pn.filter != "" {
			pn.filter = ""
			t.relist(pn)
			return nil
		}
		pn.marks = map[string]bool{}
	case "space":
		if e, ok := pn.entry(); ok {
			if pn.marks[e.Name] {
				delete(pn.marks, e.Name)
			} else {
				pn.marks[e.Name] = true
			}
			return move(pn.cur + 1)
		}
	case "a":
		if len(pn.marks) > 0 {
			pn.marks = map[string]bool{}
		} else {
			for _, e := range pn.rows {
				pn.marks[e.Name] = true
			}
		}
	case "/":
		pn.filter = "\x00"
		t.relist(pn)
	case ".":
		t.hidden = !t.hidden
		t.relist(t.panes[0])
		t.relist(t.panes[1])
		t.saveUI()
	case "s":
		t.sortBy = map[string]string{sortName: sortMod, sortMod: sortSize, sortSize: sortName}[t.sortBy]
		t.relist(t.panes[0])
		t.relist(t.panes[1])
		t.saveUI()
		return toast("sorted by "+t.sortBy, false)
	case "p":
		t.preview = !t.preview
		t.saveUI()
		return t.rest()
	case "L":
		t.dual = !t.dual
		if t.dual {
			t.side = sideHere
		} else {
			t.side = sideHost
		}
		t.saveUI()
		return tea.Batch(t.list(sideHere, t.panes[sideHere].path, c), t.rest())
	case "~":
		if pn.side == sideHost {
			return t.goTo(pn, cmp.Or(t.home, "~"), c)
		}
		return t.goTo(pn, t.hereHome, c)
	case "g":
		t.ask = newAskLine("Go to a folder on "+t.sideName(pn.side), "~/code", t.shownPath(pn), func(v string, c *Ctx) tea.Cmd {
			if pn.side == sideHere {
				v = t.expandHere(v)
			}
			return t.goTo(pn, v, c)
		})
	case "R", "ctrl+r":
		return t.list(pn.side, pn.path, c)
	case "y":
		if e, ok := pn.entry(); ok {
			p := pn.join(e.Name)
			return tea.Batch(tea.SetClipboard(p), toast("copied "+p, false))
		}
	case "d":
		if pn.side == sideHost {
			to := t.downDir
			if t.dual {
				to = t.panes[sideHere].path
			}
			return t.download(pn.picked(), to, false, c)
		}
	case "u":
		if pn.side == sideHere {
			return t.upload(pn.paths(), c)
		}
		if !t.dual {
			t.dual, t.side = true, sideHere
			t.saveUI()
			return tea.Batch(t.list(sideHere, t.panes[sideHere].path, c), toast("choose what goes up here, then u (or drop files on the window)", false))
		}
		t.side = sideHere
	case "c", "f5":
		if !t.dual {
			return toast("L shows this machine's side too; c copies across", false)
		}
		if pn.side == sideHost {
			return t.download(pn.picked(), t.panes[sideHere].path, false, c)
		}
		return t.upload(pn.paths(), c)
	case "o":
		e, ok := pn.entry()
		if !ok {
			return nil
		}
		if pn.side == sideHere || t.local {
			openHere(pn.join(e.Name))
			return toast("opened "+e.Name, false)
		}
		return t.download([]fleetapi.FileEntry{e}, filepath.Join(openDir(), hostfiles.NewUploadID()[:8]), true, c)
	case "D":
		t.ask = newAskLine("Downloads go to (on this machine)", "~/Downloads", t.tildeHere(t.downDir), func(v string, c *Ctx) tea.Cmd {
			t.downDir = t.expandHere(v)
			t.saveUI()
			return toast("downloads go to "+t.tildeHere(t.downDir), false)
		})
	case "n":
		t.ask = newAskLine("New folder in "+t.shownPath(pn), "name", "", func(v string, c *Ctx) tea.Cmd {
			return t.op(pn.side, fleetapi.FileOp{Op: fleetapi.OpMkdir, Path: pn.join(v)}, "made "+v, v, c)
		})
	case "r":
		e, ok := pn.entry()
		if !ok {
			return nil
		}
		t.ask = newAskLine("Rename "+e.Name, "new name", e.Name, func(v string, c *Ctx) tea.Cmd {
			if v == e.Name {
				return nil
			}
			return t.op(pn.side, fleetapi.FileOp{Op: fleetapi.OpRename, Path: pn.join(e.Name), To: v}, "renamed to "+v, v, c)
		})
	case "x", "delete", "X":
		es := pn.picked()
		if len(es) == 0 {
			return nil
		}
		var paths []string
		for _, e := range es {
			paths = append(paths, pn.join(e.Name))
		}
		what := es[0].Name
		if len(es) > 1 {
			what = fmt.Sprintf("these %d", len(es))
		}
		op, ask, say := fleetapi.OpTrash, fmt.Sprintf("Move %s to the trash on %s? fleetd keeps it a week.", what, t.sideName(pn.side)), "in the trash: "+what
		if pn.side == sideHere {
			ask = fmt.Sprintf("Move %s to this machine's trash?", what)
		}
		if k == "X" {
			op, ask, say = fleetapi.OpDelete, fmt.Sprintf("Delete %s on %s for good? It cannot be brought back.", what, t.sideName(pn.side)), "deleted "+what
		}
		t.confirm = &confirm{ask: ask, act: t.op(pn.side, fleetapi.FileOp{Op: op, Paths: paths}, say, "", c)}
	case "T":
		t.panel = &xferPanel{}
	case "ctrl+v":
		if c.Clipboard == nil {
			return toast("no clipboard here to paste from", true)
		}
		return t.clipboard(c)
	}
	return nil
}

func (p *fpane) paths() []string {
	var out []string
	for _, e := range p.picked() {
		out = append(out, p.join(e.Name))
	}
	return out
}

func (t *filesTab) filterKey(m tea.KeyPressMsg, c *Ctx) tea.Cmd {
	pn := t.pane()
	f := strings.TrimSuffix(pn.filter, "\x00")
	switch k := m.String(); k {
	case "esc":
		pn.filter = ""
	case "enter":
		pn.filter = f // kept, no longer typed into
	case "backspace":
		if f == "" {
			pn.filter = ""
		} else {
			r := []rune(f)
			pn.filter = string(r[:len(r)-1]) + "\x00"
		}
	case "up", "down":
		pn.filter = f
		return t.key(m, c)
	default:
		if m.Text != "" {
			pn.filter = f + m.Text + "\x00"
		}
	}
	pn.cur, pn.top = 0, 0
	t.relist(pn)
	return t.rest()
}

func (t *filesTab) sideName(side int) string {
	if side == sideHere {
		return "this machine"
	}
	return cmp.Or(t.host, "fleetd's machine")
}

func (t *filesTab) shownPath(pn *fpane) string {
	if pn.side == sideHere {
		return t.tildeHere(pn.path)
	}
	if t.home != "" && (pn.path == t.home || strings.HasPrefix(pn.path, t.home+"/")) {
		return "~" + pn.path[len(t.home):]
	}
	return pn.path
}

func (t *filesTab) expandHere(v string) string {
	if v == "~" {
		return t.hereHome
	}
	if strings.HasPrefix(v, "~/") {
		return filepath.Join(t.hereHome, v[2:])
	}
	if !filepath.IsAbs(v) {
		return filepath.Join(t.panes[sideHere].path, v)
	}
	return filepath.Clean(v)
}

// op changes files on a side, and lists its folder again.
func (t *filesTab) op(side int, op fleetapi.FileOp, say, focus string, c *Ctx) tea.Cmd {
	cl := c.Client
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		var err error
		if side == sideHere {
			err = hereOp(op)
		} else {
			_, err = cl.FilesOp(ctx, op)
		}
		return filesOpMsg{side: side, say: say, err: err, focus: focus}
	}
}

// download fetches entries of fleetd's into dir here.
func (t *filesTab) download(es []fleetapi.FileEntry, dir string, open bool, c *Ctx) tea.Cmd {
	pn := t.panes[sideHost]
	var xs []*xfer
	for _, e := range es {
		if e.Kind == fleetapi.KindOther {
			continue
		}
		xs = append(xs, &xfer{name: e.Name, src: pn.join(e.Name), dst: dir, dir: e.IsDir(), open: open})
	}
	if len(xs) == 0 {
		return nil
	}
	pn.marks = map[string]bool{}
	what := xs[0].name
	if len(xs) > 1 {
		what = fmt.Sprintf("%d items", len(xs))
	}
	say := "downloading " + what + " to " + t.tildeHere(dir)
	if open {
		say = "fetching " + what + " to open it here"
	}
	return tea.Batch(t.queue(c, xs...), toast(say, false))
}

// upload sends paths here into the folder shown on fleetd's side.
func (t *filesTab) upload(paths []string, c *Ctx) tea.Cmd {
	dst := t.panes[sideHost].path
	var xs []*xfer
	for _, p := range paths {
		fi, err := os.Stat(p)
		if err != nil {
			return toast(oneLine(err.Error()), true)
		}
		xs = append(xs, &xfer{up: true, name: filepath.Base(p), src: p, dst: dst, dir: fi.IsDir()})
	}
	if len(xs) == 0 {
		return nil
	}
	t.panes[sideHere].marks = map[string]bool{}
	what := xs[0].name
	if len(xs) > 1 {
		what = fmt.Sprintf("%d items", len(xs))
	}
	return tea.Batch(t.queue(c, xs...), toast("uploading "+what+" to "+t.shownPath(t.panes[sideHost]), false))
}

// paste: files dropped on the window (or copied in a file manager) go up
// into the folder shown on fleetd's side; other text goes to a field.
func (t *filesTab) paste(m tea.PasteMsg, c *Ctx) tea.Cmd {
	if t.ask != nil {
		var cmd tea.Cmd
		t.ask.in, cmd = t.ask.in.Update(m)
		return cmd
	}
	if t.filtering() {
		pn := t.pane()
		pn.filter = strings.TrimSuffix(pn.filter, "\x00") + oneLine(m.Content) + "\x00"
		t.relist(pn)
		return nil
	}
	if paths := localPaths(m.Content); len(paths) > 0 {
		return t.upload(paths, c)
	}
	return toast("paste files (drop them on the window, or copy them in Finder) to upload them here", false)
}

// clipboard reads this machine's clipboard: files, or an image kept as a
// file, go up.
func (t *filesTab) clipboard(c *Ctx) tea.Cmd {
	clip := c.Clipboard
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		dir := filepath.Join(os.TempDir(), "fleetctl-clip")
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return filesClipMsg{err: err}
		}
		img := filepath.Join(dir, "clipboard-"+time.Now().Format("20060102-150405")+".png")
		got, err := clip.Read(ctx, img)
		switch {
		case err != nil:
			return filesClipMsg{err: fmt.Errorf("the clipboard: %w", err)}
		case len(got.Files) > 0:
			return filesClipMsg{paths: got.Files}
		case got.Image != "":
			return filesClipMsg{paths: []string{got.Image}}
		}
		return filesClipMsg{}
	}
}

// filesClipMsg is the clipboard read: the files to send up.
type filesClipMsg struct {
	paths []string
	err   error
}

// clipped is the clipboard read: its files, or its image, go up.
func (t *filesTab) clipped(m filesClipMsg, c *Ctx) tea.Cmd {
	switch {
	case m.err != nil:
		return toast(oneLine(m.err.Error()), true)
	case len(m.paths) == 0:
		return toast("nothing on the clipboard to upload: copy a file or an image", false)
	}
	return t.upload(m.paths, c)
}

// hereOp is an operation on this machine's files.
func hereOp(op fleetapi.FileOp) error {
	switch op.Op {
	case fleetapi.OpMkdir:
		return os.Mkdir(op.Path, 0o755)
	case fleetapi.OpRename:
		to := op.To
		if !strings.ContainsRune(to, filepath.Separator) {
			to = filepath.Join(filepath.Dir(op.Path), to)
		}
		if _, err := os.Lstat(to); err == nil {
			return fmt.Errorf("%s is already there", to)
		}
		return os.Rename(op.Path, to)
	case fleetapi.OpTrash:
		for _, p := range op.Paths {
			if err := trashHere(p); err != nil {
				return err
			}
		}
	case fleetapi.OpDelete:
		for _, p := range op.Paths {
			if err := os.RemoveAll(p); err != nil {
				return err
			}
		}
	}
	return nil
}
