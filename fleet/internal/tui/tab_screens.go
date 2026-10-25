package tui

import (
	"context"
	"slices"
	"strings"
	"time"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"

	"github.com/MelloB1989/karmax/fleet/internal/fleetapi"
)

const (
	screensEvery   = 2 * time.Second  // how often the visible tiles are captured again
	screensLost    = 10 * time.Second // a capture this old with no answer is given up on
	tileMinW       = 60               // the grid has width ÷ tileMinW columns
	tileMinH       = 9                // and height ÷ tileMinH rows
	screensTimeout = 8 * time.Second
)

// screens is the Screens tab: a grid of live previews of the workers'
// sessions. Only the tiles in view are captured, every 2 s while the tab is
// shown; ⏎ zooms one tile to the whole body.
type screens struct {
	sel    int    // index into the snapshot's workers
	zoom   string // the zoomed worker, or ""
	off    int    // the first row of tiles in view
	w, h   int    // the body's size, from the last View (or the window's)
	got    map[string]fleetapi.Screen
	asked  []string // the workers of the capture in flight or last made
	lines  int
	flying bool
	sentAt time.Time // when the capture in flight (or the last one) was asked for
	err    string
}

func newScreens() Tab { return &screens{got: map[string]fleetapi.Screen{}} }

type (
	screensMsg struct {
		asked []string
		got   []fleetapi.Screen
		err   error
	}
	screensTickMsg struct{}
)

func (s *screens) Title() string               { return "Screens" }
func (s *screens) Visible(me fleetapi.Me) bool { return true }
func (s *screens) Capturing() bool             { return false }

var (
	screensKeys = struct{ Move, Zoom, Back, SSH, Type key.Binding }{
		Move: key.NewBinding(key.WithKeys("up", "down", "left", "right", "k", "j", "h", "l"), key.WithHelp("←↑↓→", "select")),
		Zoom: key.NewBinding(key.WithKeys("enter"), key.WithHelp("⏎", "zoom")),
		Back: key.NewBinding(key.WithKeys("esc"), key.WithHelp("esc", "grid")),
		SSH:  key.NewBinding(key.WithKeys("s"), key.WithHelp("s", "ssh")),
		Type: key.NewBinding(key.WithKeys("t"), key.WithHelp("t", "type")),
	}
)

func (s *screens) Help() []key.Binding {
	k := screensKeys
	return []key.Binding{k.Move, k.Zoom, k.Back, k.SSH, k.Type}
}

func snapWorkers(c *Ctx) []fleetapi.Worker {
	if c.Snap == nil {
		return nil
	}
	return c.Snap.Workers
}

// tileGrid is the tile layout for a w×h body: columns, rows and tile height.
func tileGrid(w, h int) (cols, rows, tileH int) {
	cols, rows = max(1, w/tileMinW), max(1, h/tileMinH)
	return cols, rows, max(1, h/rows)
}

// visible is the workers whose tiles are in view, and how many lines each
// tile holds.
func (s *screens) visible(c *Ctx) ([]string, int) {
	ws := snapWorkers(c)
	if s.w <= 0 || s.h <= 0 || len(ws) == 0 {
		return nil, 0
	}
	if s.zoom != "" {
		return []string{s.zoom}, max(1, s.h-2)
	}
	cols, rows, tileH := tileGrid(s.w, s.h)
	s.sel = min(max(s.sel, 0), len(ws)-1)
	s.off = keepInView(s.off, s.sel/cols, (len(ws)+cols-1)/cols, rows)
	var out []string
	for i := s.off * cols; i < min(len(ws), (s.off+rows)*cols); i++ {
		out = append(out, ws[i].Agent)
	}
	return out, max(1, tileH-2)
}

func (s *screens) Update(msg tea.Msg, c *Ctx) tea.Cmd {
	ws := snapWorkers(c)
	if c.Selected != "" && (s.sel >= len(ws) || ws[s.sel].Agent != c.Selected) {
		s.follow(c.Selected, ws) // another tab selected a worker: show it
	}
	var cmds []tea.Cmd
	switch m := msg.(type) {
	case tea.WindowSizeMsg:
		if s.w == 0 { // until the first View: about the body's size
			s.w, s.h = m.Width, max(0, m.Height-6)
		}
	case screensMsg:
		s.flying = false
		if m.err != nil {
			s.err = m.err.Error()
		} else {
			s.err = ""
			for _, sc := range m.got {
				s.got[sc.Worker] = sc
			}
		}
		cmds = append(cmds, tea.Tick(screensEvery, func(time.Time) tea.Msg { return screensTickMsg{} }))
	case tea.KeyPressMsg:
		cmds = append(cmds, s.key(m.String(), c, ws))
	}
	return tea.Batch(append(cmds, s.fetch(c))...)
}

// follow selects worker w if it is in the fleet.
func (s *screens) follow(w string, ws []fleetapi.Worker) {
	if i := slices.IndexFunc(ws, func(x fleetapi.Worker) bool { return x.Agent == w }); i >= 0 {
		s.sel = i
	}
}

func (s *screens) key(k string, c *Ctx, ws []fleetapi.Worker) tea.Cmd {
	if len(ws) == 0 {
		return nil
	}
	cols, _, _ := tileGrid(s.w, s.h)
	s.sel = min(max(s.sel, 0), len(ws)-1)
	switch k {
	case "up", "k":
		s.sel = max(0, s.sel-cols)
	case "down", "j":
		s.sel = min(len(ws)-1, s.sel+cols)
	case "left", "h":
		s.sel = max(0, s.sel-1)
	case "right", "l":
		s.sel = min(len(ws)-1, s.sel+1)
	case "enter":
		if s.zoom != "" {
			s.zoom = ""
		} else {
			s.zoom = ws[s.sel].Agent
		}
	case "esc":
		s.zoom = ""
	case "s", "t":
		mode := fleetapi.TermWatch
		if k == "t" {
			mode = fleetapi.TermType
		}
		w := ws[s.sel].Agent
		if s.zoom != "" {
			w = s.zoom
		}
		if c.Attach == nil {
			return toastCmd("ssh is not available here", true)
		}
		return c.Attach(w, mode)
	}
	if s.zoom != "" && k != "enter" && k != "esc" {
		s.zoom = ws[s.sel].Agent // moving while zoomed moves the zoom
	}
	c.Selected = ws[s.sel].Agent
	return nil
}

// fetch captures the visible tiles when the last capture is 2 s old or no
// longer covers them, and nothing is in flight.
func (s *screens) fetch(c *Ctx) tea.Cmd {
	want, lines := s.visible(c)
	if len(want) == 0 || c.Client == nil {
		return nil
	}
	same := slices.Equal(want, s.asked) && lines == s.lines
	if s.flying && c.Now.Sub(s.sentAt) < screensLost {
		return nil // its answer refetches if the tiles have moved meanwhile
	}
	if same && c.Now.Sub(s.sentAt) < screensEvery {
		return nil
	}
	s.asked, s.lines, s.flying, s.sentAt = want, lines, true, c.Now
	cl := c.Client
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), screensTimeout)
		defer cancel()
		got, err := cl.Screens(ctx, want, lines)
		return screensMsg{asked: want, got: got, err: err}
	}
}

func (s *screens) View(c *Ctx, w, h int) string {
	s.w, s.h = w, h
	ws := snapWorkers(c)
	switch {
	case c.Snap == nil:
		return centerNote(c.Theme, "waiting for fleetd…", w, h)
	case len(ws) == 0:
		return centerNote(c.Theme, "no agents to show", w, h)
	}
	if s.zoom != "" {
		i := slices.IndexFunc(ws, func(x fleetapi.Worker) bool { return x.Agent == s.zoom })
		if i < 0 {
			s.zoom = ""
		} else {
			return strings.Join(s.tile(c, ws[i], true, w, h), "\n")
		}
	}
	names, _ := s.visible(c)
	cols, rows, tileH := tileGrid(w, h)
	tileW := w / cols
	var out []string
	for r := 0; r < rows; r++ {
		row := make([]string, tileH)
		for k := 0; k < cols; k++ {
			i := s.off*cols + r*cols + k
			tw := tileW
			if k == cols-1 {
				tw = w - tileW*(cols-1) // the last column takes the remainder
			}
			var t []string
			if r*cols+k < len(names) {
				t = s.tile(c, ws[i], i == s.sel, tw, tileH)
			} else {
				t = strings.Split(fillBody(nil, tw, tileH), "\n")
			}
			for j := range row {
				row[j] += t[j]
			}
		}
		out = append(out, row...)
	}
	return fillBody(out, w, h)
}

// tile is one worker's preview, exactly w×h.
func (s *screens) tile(c *Ctx, wk fleetapi.Worker, sel bool, w, h int) []string {
	th := c.Theme
	col, phase := th.Phase(wk.State, wk.Alerts)
	title := Sprite(th, wk.Activity.Kind, c.Frame) + " " + th.Strong.Render(wk.Agent) + " " +
		th.Muted.Render(phase)
	if wk.Task != "" {
		title += " " + th.Muted.Render("· "+wk.Task)
	}
	if sel {
		col = th.Amber
	}
	in := max(0, w-2)
	var body []string
	sc, ok := s.got[wk.Agent]
	switch {
	case ok && sc.Error != "":
		body = []string{th.Danger.Render(sc.Error)}
	case ok:
		lines := strings.Split(strings.TrimRight(sc.Text, "\n"), "\n")
		if n := max(0, h-2); len(lines) > n {
			lines = lines[len(lines)-n:]
		}
		for _, l := range lines {
			body = append(body, rawLine(l, in))
		}
	case s.err != "":
		body = []string{th.Danger.Render(s.err)}
	default:
		body = []string{th.Muted.Render("…")}
	}
	return tileBox(th, title, col, sel, body, w, h)
}
