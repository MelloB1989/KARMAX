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
// JSON as text frames. ?control=1 takes the keyboard (409 with the holder
// when someone has it); binary frames from the client are keystrokes.
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
		if control {
			who := r.URL.Query().Get("who")
			if who == "" {
				who = "client"
			}
			var err error
			if release, err = t.acquire(agent, who); err != nil {
				var held ErrHeld
				if errors.As(err, &held) {
					writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error(), "holder": held.Holder})
					return
				}
				writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
				return
			}
		}
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			if release != nil {
				release()
			}
			return
		}
		defer c.CloseNow()
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
			ctx = c.CloseRead(ctx)
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
