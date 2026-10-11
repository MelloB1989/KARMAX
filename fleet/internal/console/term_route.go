package console

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"
)

// term is GET /v1/term/{agent}: terminal bytes as binary frames, TermFrame
// JSON as text frames. ?control=1 takes the keyboard; binary frames from the
// client are keystrokes. When someone else has it the socket is still
// upgraded (a browser cannot read an HTTP error from a failed upgrade): one
// {"type":"error","holder":…} frame, then close. A watching client may send
// {"type":"redraw"} to have tmux repaint the shared screen for a viewer that
// joined mid-stream. Both kinds are pinged, and a socket that stops
// answering is dropped, which releases the keyboard.
func term(t *Terms) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		agent := r.PathValue("agent")
		conf, ok := t.conf(agent)
		if !ok {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "no live agent " + agent})
			return
		}
		control := r.URL.Query().Get("control") == "1"
		var release func()
		var acqErr error
		if control {
			who := r.URL.Query().Get("who")
			if who == "" {
				who = "client"
			}
			release, acqErr = t.acquire(agent, who)
		}
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			if release != nil {
				release()
			}
			return
		}
		defer c.CloseNow()
		if acqErr != nil {
			f := TermFrame{Type: "error", Agent: agent, Mode: "control", Error: acqErr.Error()}
			var held ErrHeld
			if errors.As(acqErr, &held) {
				f.Holder = held.Holder
			}
			b, _ := json.Marshal(f)
			wctx, wc := context.WithTimeout(r.Context(), 5*time.Second)
			c.Write(wctx, websocket.MessageText, b)
			wc()
			c.Close(websocket.StatusPolicyViolation, acqErr.Error())
			return
		}
		ctx, cancel := context.WithCancel(r.Context())
		defer cancel()

		var wmu sync.Mutex
		send := func(typ websocket.MessageType, b []byte) {
			wmu.Lock()
			defer wmu.Unlock()
			wctx, wc := context.WithTimeout(ctx, 5*time.Second)
			defer wc()
			if c.Write(wctx, typ, b) != nil {
				cancel() // a client that cannot be written to is gone
			}
		}
		out := func(b []byte) { send(websocket.MessageBinary, b) }
		frame := func(f TermFrame) {
			b, _ := json.Marshal(f)
			send(websocket.MessageText, b)
		}

		go keepAlive(ctx, c, cancel)

		var runErr error
		if control {
			in := make(chan []byte, 64)
			go func() { // keystrokes; the socket closing ends the control
				defer cancel()
				for {
					typ, b, err := c.Read(ctx)
					if err != nil {
						return
					}
					if typ == websocket.MessageBinary {
						select {
						case in <- b:
						case <-ctx.Done():
							return
						}
					}
				}
			}()
			runErr = t.control(ctx, conf, release, in, out, frame)
		} else {
			c.SetReadLimit(4096)
			go func() { // a watcher says nothing but "redraw"; the socket closing ends the watch
				defer cancel()
				for {
					typ, b, err := c.Read(ctx)
					if err != nil {
						return
					}
					if typ != websocket.MessageText {
						continue
					}
					var f TermFrame
					if json.Unmarshal(b, &f) == nil && f.Type == "redraw" {
						t.Redraw(agent)
					}
				}
			}()
			runErr = t.Watch(ctx, agent, out, frame)
		}
		switch {
		case runErr == nil, errors.Is(runErr, context.Canceled):
			c.Close(websocket.StatusNormalClosure, "")
		case errors.Is(runErr, ErrSlow):
			c.Close(websocket.StatusTryAgainLater, "too slow: resubscribe")
		default:
			frame(TermFrame{Type: "error", Agent: agent, Error: runErr.Error()})
			c.Close(websocket.StatusInternalError, "terminal failed")
		}
	}
}

// Ping cadence and the wait for the pong. A client that vanished without a
// FIN (a closed laptop, a dead VPN) would otherwise hold the keyboard until
// TCP gave up.
var (
	pingEvery   = 30 * time.Second
	pingTimeout = 10 * time.Second
)

// keepAlive pings c until ctx ends and calls drop when a ping goes
// unanswered. It needs a reader running on c, which both term modes have.
func keepAlive(ctx context.Context, c *websocket.Conn, drop context.CancelFunc) {
	tk := time.NewTicker(pingEvery)
	defer tk.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tk.C:
			pctx, cancel := context.WithTimeout(ctx, pingTimeout)
			err := c.Ping(pctx)
			cancel()
			if err != nil && ctx.Err() == nil {
				drop()
				return
			}
		}
	}
}
