package tui

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	tea "charm.land/bubbletea/v2"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/vt"

	"github.com/MelloB1989/karmax/fleet/internal/fleetapi"
)

// A desk session's terminal in the TUI: an emulator of its own, kept for
// as long as the TUI runs, so that coming back to a session shows it at
// once; and its connection to fleetd, kept while it is shown and for a
// while after, so that switching costs nothing on a slow link.

// The terminals' keeping: a session hidden this long, or beyond the most
// recent warmKeep hidden ones, lets its connection go (its screen stays).
var (
	warmFor  = 5 * time.Minute
	warmKeep = 10
)

// Reconnects back off from minBackTerm to maxBackTerm.
var (
	minBackTerm = 500 * time.Millisecond
	maxBackTerm = 8 * time.Second
)

const paintGap = 12 * time.Millisecond // a burst of output is one paint

type termState int

const (
	termIdle       termState = iota // not connected
	termConnecting                  // dialing, or waiting for fleetd's hello
	termLive                        // typing goes to it
	termHeld                        // another terminal types in it (holder)
	termReleased                    // another terminal took the keyboard: this one watches
	termDown                        // the connection failed; it is tried again
	termEnded                       // its terminal ended (the session is gone)
)

// deskTerm is one session's terminal.
type deskTerm struct {
	name string
	emu  *vt.SafeEmulator
	out  chan []byte // what the emulator sends (keys, its answers), for the connection

	mu      sync.Mutex
	state   termState
	holder  string // termHeld, termReleased: who types
	err     string // termDown, termEnded: why
	conn    fleetapi.TermConn
	cancel  context.CancelFunc
	gen     int  // the connection's generation: an older one's news is dropped
	held    bool // this TUI had the keyboard: a reconnect takes it back
	back    time.Duration
	retry   *time.Timer
	want    bool      // the manager wants it connected
	shown   time.Time // when it was last on screen
	visible bool
}

// status is the terminal's state, holder and error.
func (t *deskTerm) status() (termState, string, string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.state, t.holder, t.err
}

// termMgr is every session's terminal.
type termMgr struct {
	cl  fleetapi.Client
	ctx context.Context

	mu    sync.Mutex
	terms map[string]*deskTerm

	kick    chan struct{} // a screen changed (cap 1)
	clips   chan string   // what a program in a session copied (OSC 52)
	waiters atomic.Int32  // paint waiters out
}

func newTermMgr(ctx context.Context, cl fleetapi.Client) *termMgr {
	return &termMgr{cl: cl, ctx: ctx, terms: map[string]*deskTerm{}, kick: make(chan struct{}, 1), clips: make(chan string, 8)}
}

func (m *termMgr) ping() {
	select {
	case m.kick <- struct{}{}:
	default:
	}
}

// deskPaintMsg: a screen changed; clips is what was copied since.
type deskPaintMsg struct{ clips []string }

// wait is a command that answers once a screen changes: one paint for a
// burst. At most two are out at a time (a message the app gave another tab
// is lost, and the tab arms another when shown).
func (m *termMgr) wait() tea.Cmd {
	if m.waiters.Load() >= 2 {
		return nil
	}
	m.waiters.Add(1)
	return func() tea.Msg {
		defer m.waiters.Add(-1)
		select {
		case <-m.kick:
		case <-m.ctx.Done():
			return nil
		}
		time.Sleep(paintGap)
		var clips []string
		for {
			select {
			case c := <-m.clips:
				clips = append(clips, c)
				continue
			default:
			}
			break
		}
		return deskPaintMsg{clips: clips}
	}
}

// get is name's terminal, made cols×rows when it is new.
func (m *termMgr) get(name string, cols, rows int) *deskTerm {
	m.mu.Lock()
	defer m.mu.Unlock()
	if t := m.terms[name]; t != nil {
		return t
	}
	t := &deskTerm{name: name, emu: vt.NewSafeEmulator(max(cols, 2), max(rows, 1)), out: make(chan []byte, 1024)}
	// tmux keeps the history (its copy mode, the wheel): the emulator's own
	// would be 10000 lines a session for nothing.
	t.emu.SetScrollbackSize(100)
	t.emu.RegisterOscHandler(52, func(data []byte) bool {
		if s, ok := osc52(data); ok {
			select {
			case m.clips <- s:
			default:
			}
			m.ping()
		}
		return true
	})
	m.terms[name] = t
	go m.drain(t)
	go m.send(t)
	return t
}

// peek is name's terminal if there is one.
func (m *termMgr) peek(name string) *deskTerm {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.terms[name]
}

// osc52 is the text an OSC 52 asks to copy ("c;<base64>"); a query (?) or
// a clear is not one.
func osc52(data []byte) (string, bool) {
	s := strings.TrimPrefix(string(data), "52;")
	_, payload, ok := strings.Cut(s, ";")
	if !ok || payload == "" || payload == "?" {
		return "", false
	}
	b, err := base64.StdEncoding.DecodeString(payload)
	if err != nil {
		return "", false
	}
	return string(b), true
}

// drain reads what the emulator sends into t.out at once: the emulator
// writes its answers under its lock, into a pipe, and a slow link must not
// hold that (nor so the screen).
func (m *termMgr) drain(t *deskTerm) {
	buf := make([]byte, 32<<10)
	for {
		n, err := t.emu.Read(buf)
		if n > 0 {
			select {
			case t.out <- append([]byte(nil), buf[:n]...):
			default: // 1024 sends behind: a dead link, typing dropped
			}
		}
		if err != nil {
			close(t.out)
			return
		}
	}
}

// send types what the emulator sends into the connection of the moment;
// with none, it is dropped.
func (m *termMgr) send(t *deskTerm) {
	for b := range t.out {
		t.mu.Lock()
		c, live := t.conn, t.state == termLive
		t.mu.Unlock()
		if c != nil && live {
			c.Send(m.ctx, b)
		}
	}
}

// connect dials t unless it is connected or dialing; force takes the
// keyboard.
func (m *termMgr) connect(t *deskTerm, force bool) {
	t.mu.Lock()
	switch t.state {
	case termConnecting, termLive, termReleased, termEnded:
		t.mu.Unlock()
		return
	case termHeld:
		if !force {
			t.mu.Unlock()
			return
		}
	}
	if t.retry != nil {
		t.retry.Stop()
		t.retry = nil
	}
	t.gen++
	gen := t.gen
	ctx, cancel := context.WithCancel(m.ctx)
	t.cancel, t.state, t.err, t.holder = cancel, termConnecting, "", ""
	force = force || t.held
	t.mu.Unlock()
	cols, rows := t.emu.Width(), t.emu.Height()
	m.ping()
	go m.run(t, ctx, gen, cols, rows, force)
}

// run is one connection: fleetd's bytes into the emulator until it ends.
func (m *termMgr) run(t *deskTerm, ctx context.Context, gen, cols, rows int, force bool) {
	conn, err := m.cl.DeskTerm(ctx, t.name, cols, rows, force)
	if err != nil {
		m.down(t, gen, err)
		return
	}
	t.mu.Lock()
	if t.gen != gen {
		t.mu.Unlock()
		conn.Close()
		return
	}
	t.conn = conn
	t.mu.Unlock()
	// The size may have changed while dialing.
	if w, h := t.emu.Width(), t.emu.Height(); w != cols || h != rows {
		conn.Resize(ctx, w, h)
	}
	for {
		data, f, err := conn.Recv(ctx)
		if err != nil {
			conn.Close()
			m.down(t, gen, err)
			return
		}
		if f == nil {
			t.emu.Write(data)
			m.ping()
			continue
		}
		t.mu.Lock()
		if t.gen != gen {
			t.mu.Unlock()
			conn.Close()
			return
		}
		switch f.Type {
		case fleetapi.FrameHello:
			t.state, t.held, t.back = termLive, true, 0
		case fleetapi.FrameHeld:
			t.state, t.holder, t.conn = termHeld, f.Holder, nil
		case fleetapi.FrameReleased:
			t.state, t.holder, t.held = termReleased, f.Holder, false
		case fleetapi.FrameExit, fleetapi.FrameError:
			t.state, t.err, t.conn = termEnded, f.Error, nil
			if t.err == "" {
				t.err = "its terminal ended"
			}
		}
		st := t.state
		t.mu.Unlock()
		m.ping()
		if st == termHeld || st == termEnded {
			conn.Close()
			return
		}
	}
}

// down is a connection lost: tried again, backing off, while wanted.
func (m *termMgr) down(t *deskTerm, gen int, err error) {
	t.mu.Lock()
	defer m.ping()
	defer t.mu.Unlock()
	if t.gen != gen || t.state == termEnded || t.state == termHeld {
		return
	}
	t.conn = nil
	if errors.Is(err, context.Canceled) && m.ctx.Err() != nil {
		t.state = termIdle
		return
	}
	t.state, t.err = termDown, oneLine(err.Error())
	t.back = min(max(t.back*2, minBackTerm), maxBackTerm)
	if t.want {
		t.retry = time.AfterFunc(t.back, func() {
			t.mu.Lock()
			again := t.want && t.state == termDown && t.gen == gen
			t.mu.Unlock()
			if again {
				m.connect(t, false)
			}
		})
	}
}

// release lets t's connection go; its screen stays.
func (m *termMgr) release(t *deskTerm) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.retry != nil {
		t.retry.Stop()
		t.retry = nil
	}
	if t.state == termIdle || t.state == termEnded {
		return
	}
	t.gen++
	if t.cancel != nil {
		t.cancel()
	}
	if t.conn != nil {
		t.conn.Close()
	}
	t.conn, t.state = nil, termIdle
}

// takeOver connects t taking the keyboard from whoever has it.
func (m *termMgr) takeOver(t *deskTerm) {
	m.release(t)
	t.mu.Lock()
	t.state, t.held = termIdle, true
	t.mu.Unlock()
	m.connect(t, true)
}

// retryNow tries a lost connection at once (the network is back, the
// operator asked).
func (m *termMgr) retryNow(t *deskTerm) {
	t.mu.Lock()
	down := t.state == termDown || t.state == termIdle
	t.mu.Unlock()
	if down {
		m.connect(t, false)
	}
}

// resize gives t a new size, and tells fleetd.
func (m *termMgr) resize(t *deskTerm, cols, rows int) {
	if cols < 2 || rows < 1 || (t.emu.Width() == cols && t.emu.Height() == rows) {
		return
	}
	t.emu.Resize(cols, rows)
	t.mu.Lock()
	c := t.conn
	t.mu.Unlock()
	if c != nil {
		go c.Resize(m.ctx, cols, rows)
	}
}

// keep follows what is on screen: those shown are connected, and those
// warm (each other workspace's focused session, ready before it is first
// shown); the rest are kept for warmFor, the most recent warmKeep of them;
// a session no longer on the desk is dropped.
func (m *termMgr) keep(shown, warm []string, onDesk map[string]bool, now time.Time) {
	m.mu.Lock()
	all := make([]*deskTerm, 0, len(m.terms))
	for name, t := range m.terms {
		if !onDesk[name] {
			delete(m.terms, name)
			go m.drop(t)
			continue
		}
		all = append(all, t)
	}
	m.mu.Unlock()
	vis := map[string]bool{}
	for _, n := range shown {
		vis[n] = true
	}
	var hidden []*deskTerm
	for _, t := range all {
		t.mu.Lock()
		if vis[t.name] {
			t.visible, t.shown, t.want = true, now, true
		} else {
			if t.visible {
				t.shown = now
			}
			t.visible = false
			hidden = append(hidden, t)
		}
		t.mu.Unlock()
	}
	ready := map[string]bool{}
	for i, n := range warm {
		if i < warmKeep {
			ready[n] = true
		}
	}
	// The most recently shown hidden ones stay warm, and those ready.
	sortByShown(hidden)
	kept := 0
	for _, t := range hidden {
		t.mu.Lock()
		keep := ready[t.name] || kept < warmKeep && now.Sub(t.shown) < warmFor && !t.shown.IsZero()
		if keep && !ready[t.name] {
			kept++
		}
		t.want = keep
		t.mu.Unlock()
		if !keep {
			m.release(t)
		}
	}
	for _, n := range append(shown, warm...) {
		if t := m.peek(n); t != nil && (vis[n] || ready[n]) {
			m.connect(t, false)
		}
	}
}

func sortByShown(ts []*deskTerm) {
	for i := 1; i < len(ts); i++ { // few: insertion sort, newest first
		for j := i; j > 0 && ts[j].shown.After(ts[j-1].shown); j-- {
			ts[j], ts[j-1] = ts[j-1], ts[j]
		}
	}
}

// drop ends a terminal for good: its session is gone.
func (m *termMgr) drop(t *deskTerm) {
	t.mu.Lock()
	t.want = false
	t.mu.Unlock()
	m.release(t)
	// The emulator's Close sets a flag its Read looks at unguarded: ending
	// its pipe ends the drain the same way, with no race.
	if pw, ok := t.emu.InputPipe().(*io.PipeWriter); ok {
		pw.CloseWithError(io.EOF)
	}
}

// key types k into t, as its program expects it (application cursor
// keys and the rest are the emulator's to know).
func (t *deskTerm) key(k tea.KeyPressMsg) {
	switch k.String() {
	case "shift+enter":
		t.emu.SendText("\x1b\r") // Claude Code's new line, as Option+Enter
		return
	}
	t.emu.SendKey(uv.KeyPressEvent{Text: k.Text, Mod: k.Mod, Code: k.Code, ShiftedCode: k.ShiftedCode, BaseCode: k.BaseCode})
}

// paste types text as a paste: bracketed when the program asked for that.
func (t *deskTerm) paste(text string) { t.emu.Paste(text) }

// mouse gives a mouse event, at a cell of t's, to its program.
func (t *deskTerm) mouse(ev uv.MouseEvent) { t.emu.SendMouse(ev) }
