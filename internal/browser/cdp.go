package browser

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/coder/websocket"
)

// cdpConn is one raw DevTools Protocol connection to a single page target —
// dialed straight at that page's own webSocketDebuggerUrl, not multiplexed
// over the browser-level endpoint via Target.attachToTarget/flatten. Every
// /json/list entry already carries that URL, so this is the plain route: one
// socket per tab, driven by hand in the same style Endpoint/Tabs already
// talk to the DevTools HTTP surface — see the package doc for why raw CDP
// instead of chromedp.
type cdpConn struct {
	conn *websocket.Conn

	nextID  atomic.Int64
	writeMu sync.Mutex

	pendingMu sync.Mutex
	pending   map[int64]chan cdpResult

	// onEvent fires from readLoop for every message that is not a reply to a
	// call — i.e. every CDP event. It must not block: fetching a response
	// body is itself a call over this same connection, so a handler that
	// needs one hands it to its own goroutine instead of making it inline.
	onEvent func(method string, params json.RawMessage)
}

type cdpResult struct {
	value json.RawMessage
	err   error
}

// cdpEnvelope is both directions of the wire protocol: a call sets id/method/
// params, a reply carries id plus result-or-error, an event carries method/
// params with no id.
type cdpEnvelope struct {
	ID     int64           `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *cdpError       `json:"error,omitempty"`
}

type cdpError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *cdpError) Error() string { return fmt.Sprintf("cdp: %s (code %d)", e.Message, e.Code) }

// cdpReadLimit is generous on purpose: a page's own response body can run to
// several MB before this package's 1MB capture cap ever gets a chance to say
// no, and the read has to succeed before that decision can be made.
const cdpReadLimit = 64 << 20

func dialCDP(ctx context.Context, wsURL string) (*cdpConn, error) {
	conn, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", wsURL, err)
	}
	conn.SetReadLimit(cdpReadLimit)
	return &cdpConn{conn: conn, pending: map[int64]chan cdpResult{}}, nil
}

// call sends one CDP command and waits for its reply. result may be nil when
// the caller does not need the return value.
func (c *cdpConn) call(ctx context.Context, method string, params, result any) error {
	id := c.nextID.Add(1)
	ch := make(chan cdpResult, 1)
	c.pendingMu.Lock()
	c.pending[id] = ch
	c.pendingMu.Unlock()
	forget := func() {
		c.pendingMu.Lock()
		delete(c.pending, id)
		c.pendingMu.Unlock()
	}

	var paramsRaw json.RawMessage
	if params != nil {
		b, err := json.Marshal(params)
		if err != nil {
			forget()
			return err
		}
		paramsRaw = b
	}
	req, err := json.Marshal(cdpEnvelope{ID: id, Method: method, Params: paramsRaw})
	if err != nil {
		forget()
		return err
	}

	c.writeMu.Lock()
	err = c.conn.Write(ctx, websocket.MessageText, req)
	c.writeMu.Unlock()
	if err != nil {
		forget()
		return err
	}

	select {
	case <-ctx.Done():
		forget()
		return ctx.Err()
	case res := <-ch:
		if res.err != nil {
			return res.err
		}
		if result != nil && len(res.value) > 0 {
			return json.Unmarshal(res.value, result)
		}
		return nil
	}
}

// readLoop dispatches every message on the connection until ctx is done or
// the socket errs. Run it in its own goroutine; it returns when there is
// nothing left to read, having failed every call still waiting on a reply.
func (c *cdpConn) readLoop(ctx context.Context) {
	defer c.failPending(fmt.Errorf("cdp: connection closed"))
	for {
		_, data, err := c.conn.Read(ctx)
		if err != nil {
			return
		}
		var env cdpEnvelope
		if err := json.Unmarshal(data, &env); err != nil {
			continue // not something we can parse — drop it, not the connection
		}
		if env.ID != 0 {
			c.pendingMu.Lock()
			ch, ok := c.pending[env.ID]
			if ok {
				delete(c.pending, env.ID)
			}
			c.pendingMu.Unlock()
			if !ok {
				continue
			}
			if env.Error != nil {
				ch <- cdpResult{err: env.Error}
			} else {
				ch <- cdpResult{value: env.Result}
			}
			continue
		}
		if env.Method != "" && c.onEvent != nil {
			c.onEvent(env.Method, env.Params)
		}
	}
}

func (c *cdpConn) failPending(err error) {
	c.pendingMu.Lock()
	pending := c.pending
	c.pending = map[int64]chan cdpResult{}
	c.pendingMu.Unlock()
	for _, ch := range pending {
		select {
		case ch <- cdpResult{err: err}:
		default:
		}
	}
}

func (c *cdpConn) Close() {
	_ = c.conn.Close(websocket.StatusNormalClosure, "")
}
