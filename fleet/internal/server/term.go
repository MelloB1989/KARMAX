package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/MelloB1989/karmax/fleet/internal/fleetapi"
	"github.com/MelloB1989/karmax/fleet/internal/term"
)

// TermOps is what the terminal and screen routes need of the fleet.
type TermOps interface {
	// Terminal is where a worker's session lives; false for no such worker.
	Terminal(worker string) (term.Target, bool)
	// RecordTerm records a fleet.term event.
	RecordTerm(ev term.Event)
}

// The agent's window when nobody types: the image's tmux.conf and agent-run
// make every session 220×50, and fleet.yaml has no size of its own.
const windowCols, windowRows = 220, 50

// watchIdle is how long a watcher may send nothing before fleetd closes it.
var watchIdle = 12 * time.Hour

// termRoutes registers GET /v1/term/{worker}: a websocket to the worker's
// session (watch, type) or a shell in its container. The caller is checked
// before the upgrade; a typist takes the worker's one keyboard.
func termRoutes(route func(string, func(http.ResponseWriter, *http.Request) (any, error), ...scope),
	long func(func(http.ResponseWriter, *http.Request) (any, error)) func(http.ResponseWriter, *http.Request) (any, error), ops Ops) {
	var locks term.Locks
	route("GET "+fleetapi.PathTerm+"{worker}", long(func(w http.ResponseWriter, r *http.Request) (any, error) {
		worker := r.PathValue("worker")
		q := r.URL.Query()
		mode := fleetapi.TermMode(q.Get("mode"))
		switch mode {
		case "":
			mode = fleetapi.TermWatch
		case fleetapi.TermWatch, fleetapi.TermType, fleetapi.TermShell:
		default:
			return nil, badRequest{fmt.Errorf("mode %q: want watch, type or shell", mode)}
		}
		if !mayUseAgent(r, worker) {
			return nil, forbidden{fmt.Errorf("%s is not yours to use", worker)}
		}
		tg, ok := ops.Terminal(worker)
		if !ok {
			return nil, notFound{fmt.Errorf("no agent named %q", worker)}
		}
		force := q.Get("force") == "1"
		if force && !isAdmin(r) {
			return nil, forbidden{fmt.Errorf("only an admin may take the keyboard from its holder")}
		}
		cols, rows := sizeParam(q.Get("cols"), 80), sizeParam(q.Get("rows"), 24)
		who := holderName(me(r))

		var lease *term.Lease
		var held error
		if mode == fleetapi.TermType {
			lease, held = locks.Take(worker, who, force)
		}
		ws, err := websocket.Accept(w, r, nil) // the default Origin check: no browser page drives it
		if err != nil {
			if lease != nil {
				lease.Release()
			}
			return streamed{}, nil // Accept wrote the response
		}
		conn := term.NewConn(r.Context(), ws)
		defer conn.Close()
		if h, ok := held.(term.ErrHeld); ok {
			conn.Frame(fleetapi.TermFrame{Type: fleetapi.FrameHeld, Worker: worker, Mode: mode, Holder: h.Holder,
				Error: fmt.Sprintf("%s types in %s: watch it, or wait", h.Holder, worker)})
			return streamed{}, nil
		}

		started := time.Now()
		ops.RecordTerm(term.Event{Who: who, Worker: worker, Mode: mode, Phase: "start"})
		res := runTerm(r.Context(), conn, tg, mode, cols, rows, lease)
		if res.Released { // an admin took the keyboard: the holder watches on
			conn.Frame(fleetapi.TermFrame{Type: fleetapi.FrameReleased, Worker: worker, Mode: fleetapi.TermWatch, Holder: locks.Holder(worker)})
			ops.RecordTerm(term.Event{Who: who, Worker: worker, Mode: mode, Phase: "end", Seconds: int64(time.Since(started).Seconds()), How: "released"})
			started, mode = time.Now(), fleetapi.TermWatch
			ops.RecordTerm(term.Event{Who: who, Worker: worker, Mode: mode, Phase: "start"})
			res = runTerm(r.Context(), conn, tg, mode, cols, rows, nil)
		}
		ops.RecordTerm(term.Event{Who: who, Worker: worker, Mode: mode, Phase: "end", Seconds: int64(time.Since(started).Seconds()), How: how(res)})
		return streamed{}, nil
	}), and(fullOnly, guest)...)
}

// runTerm runs one session of mode on tg until it ends, and leaves the
// worker as it found it: what the killed exec left in the container ended,
// and the window back at its size after a typist.
func runTerm(ctx context.Context, conn *term.Conn, tg term.Target, mode fleetapi.TermMode, cols, rows int, lease *term.Lease) term.Result {
	id := newTermID()
	s := &term.Session{Argv: term.Argv(tg, mode, id), Cols: cols, Rows: rows, Input: mode != fleetapi.TermWatch,
		Hello: fleetapi.TermFrame{Type: fleetapi.FrameHello, Worker: tg.Worker, Mode: mode}}
	var sizer *windowSizer
	if mode == fleetapi.TermWatch {
		s.Idle = watchIdle
	}
	if lease != nil {
		s.Released = lease.Released()
		sizer = newWindowSizer(tg)
		sizer.set(cols, rows)
		s.Resized = sizer.set
	}
	res := s.Run(ctx, conn)
	if res.Killed {
		term.Cleanup(tg, id)
	}
	if lease != nil {
		sizer.stop()
		lease.Release()
		if !res.Released { // a new typist sizes it now
			sizer.resize(windowCols, windowRows)
		}
	}
	return res
}

func how(r term.Result) string {
	switch {
	case r.Released:
		return "released"
	case r.Idle:
		return "idle"
	case r.Gone:
		return "gone"
	case r.Err != nil:
		return "error"
	}
	return "exit"
}

// holderName is who the held frame names: the login, and its device.
func holderName(m fleetapi.Me) string {
	if m.Device != "" {
		return m.Name + " (" + m.Device + ")"
	}
	return m.Name
}

func sizeParam(s string, def int) int {
	n, err := strconv.Atoi(s)
	if err != nil || n <= 0 {
		return def
	}
	return min(n, 1000)
}

func newTermID() string {
	var b [6]byte
	rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// windowSizer resizes a typist's tmux window, one exec at a time, the latest
// size winning: a terminal dragged wider sends many.
type windowSizer struct {
	tg   term.Target
	mu   sync.Mutex
	want [2]int
	kick chan struct{}
	done chan struct{}
}

func newWindowSizer(tg term.Target) *windowSizer {
	z := &windowSizer{tg: tg, kick: make(chan struct{}, 1), done: make(chan struct{})}
	go func() {
		defer close(z.done)
		for range z.kick {
			z.mu.Lock()
			c, r := z.want[0], z.want[1]
			z.mu.Unlock()
			z.resize(c, r)
		}
	}()
	return z
}

func (z *windowSizer) set(cols, rows int) {
	z.mu.Lock()
	z.want = [2]int{cols, rows}
	z.mu.Unlock()
	select {
	case z.kick <- struct{}{}:
	default:
	}
}

// stop waits for the last resize to land.
func (z *windowSizer) stop() {
	close(z.kick)
	<-z.done
}

func (z *windowSizer) resize(cols, rows int) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	z.tg.Container.Run(ctx, "tmux", "resize-window", "-t", "="+z.tg.Pane+":", "-x", strconv.Itoa(cols), "-y", strconv.Itoa(rows))
}
