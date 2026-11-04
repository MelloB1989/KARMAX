package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/MelloB1989/karmax/fleet/internal/fleetapi"
)

// The desk's calls from the TUI: a long poll for its changes, workspaces,
// pastes and terminals, on one transport kept between calls, so a slow
// link pays for its handshakes once.

// deskWait is how long a DeskWatch lets fleetd wait for a change.
const deskWait = 45

var (
	deskTransportOnce sync.Once
	deskTransport     *http.Transport
)

func deskHTTP() *http.Client {
	deskTransportOnce.Do(func() {
		deskTransport = &http.Transport{Proxy: http.ProxyFromEnvironment,
			DialContext:         (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
			MaxIdleConnsPerHost: 8, IdleConnTimeout: 2 * time.Minute}
	})
	return &http.Client{Transport: deskTransport}
}

func (a *apiClient) DeskWatch(ctx context.Context, since string) (*fleetapi.DeskList, error) {
	var l fleetapi.DeskList
	q := url.Values{"wait": {strconv.Itoa(deskWait)}}
	if since != "" {
		q.Set("since", since)
	}
	if err := a.do(ctx, "GET", fleetapi.PathDesk+"?"+q.Encode(), nil, &l, (deskWait+20)*time.Second); err != nil {
		return nil, err
	}
	return &l, nil
}

func (a *apiClient) DeskUpdate(ctx context.Context, name string, u fleetapi.DeskUpdate) error {
	return a.do(ctx, "PATCH", fleetapi.PathDesk+"/"+url.PathEscape(name), u, nil, callTimeout)
}

func (a *apiClient) DeskWorkspaceCreate(ctx context.Context, c fleetapi.DeskWorkspaceCreate) (*fleetapi.DeskWorkspace, error) {
	var w fleetapi.DeskWorkspace
	if err := a.do(ctx, "POST", fleetapi.PathDeskWorkspaces, c, &w, callTimeout); err != nil {
		return nil, err
	}
	return &w, nil
}

func (a *apiClient) DeskWorkspaceUpdate(ctx context.Context, id string, u fleetapi.DeskWorkspaceUpdate) error {
	return a.do(ctx, "PATCH", fleetapi.PathDeskWorkspaces+"/"+url.PathEscape(id), u, nil, callTimeout)
}

func (a *apiClient) DeskWorkspaceDelete(ctx context.Context, id string, end bool) error {
	p := fleetapi.PathDeskWorkspaces + "/" + url.PathEscape(id)
	if end {
		p += "?end=1"
	}
	return a.do(ctx, "DELETE", p, nil, nil, callTimeout)
}

// DeskPaste streams body to fleetd: no time limit but ctx's, as a big file
// on a slow link takes what it takes.
func (a *apiClient) DeskPaste(ctx context.Context, session, file string, body io.Reader, size int64, tar bool) (*fleetapi.DeskPasted, error) {
	q := url.Values{"session": {session}, "file": {file}}
	if tar {
		q.Set("kind", "tar")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.c.base+fleetapi.PathDeskPaste+"?"+q.Encode(), body)
	if err != nil {
		return nil, err
	}
	req.ContentLength = size
	req.Header.Set("Content-Type", "application/octet-stream")
	if a.c.token != "" {
		req.Header.Set("Authorization", "Bearer "+a.c.token)
	}
	resp, err := deskHTTP().Do(req)
	if err != nil {
		return nil, fmt.Errorf("fleetd unreachable at %s: %w", a.c.base, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 300 {
		var e struct{ Error string }
		if json.Unmarshal(b, &e) == nil && e.Error != "" {
			return nil, &statusError{Code: resp.StatusCode, msg: e.Error, Body: b}
		}
		return nil, &statusError{Code: resp.StatusCode, msg: "fleetd: " + resp.Status, Body: b}
	}
	var p fleetapi.DeskPasted
	return &p, json.Unmarshal(b, &p)
}

// DeskTerm types in a desk session over GET /v1/term/desk:<name>, its
// bytes deflated both ways when fleetd agrees (a slow link).
func (a *apiClient) DeskTerm(ctx context.Context, name string, cols, rows int, force bool) (fleetapi.TermConn, error) {
	q := url.Values{"mode": {string(fleetapi.TermType)}, "cols": {strconv.Itoa(cols)}, "rows": {strconv.Itoa(rows)}}
	if force {
		q.Set("force", "1")
	}
	u := "ws" + strings.TrimPrefix(a.c.base, "http") + fleetapi.PathTerm + url.PathEscape(fleetapi.DeskWorker(name)) + "?" + q.Encode()
	h := http.Header{}
	if a.c.token != "" {
		h.Set("Authorization", "Bearer "+a.c.token)
	}
	dctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	ws, resp, err := websocket.Dial(dctx, u, &websocket.DialOptions{HTTPHeader: h, HTTPClient: deskHTTP(),
		CompressionMode: websocket.CompressionContextTakeover})
	if err != nil {
		return nil, termDialError(a.c.base, resp, err)
	}
	ws.SetReadLimit(16 << 20)
	return &wsTerm{ws: ws}, nil
}

// wsTerm is a terminal session on its websocket: bytes as binary frames,
// control as JSON text frames.
type wsTerm struct {
	ws  *websocket.Conn
	wmu sync.Mutex
}

func (t *wsTerm) Recv(ctx context.Context) ([]byte, *fleetapi.TermFrame, error) {
	typ, b, err := t.ws.Read(ctx)
	if err != nil {
		return nil, nil, err
	}
	if typ == websocket.MessageBinary {
		return b, nil, nil
	}
	var f fleetapi.TermFrame
	if err := json.Unmarshal(b, &f); err != nil {
		return nil, nil, fmt.Errorf("fleetd sent %q: %w", b, err)
	}
	return nil, &f, nil
}

func (t *wsTerm) write(ctx context.Context, typ websocket.MessageType, b []byte) error {
	t.wmu.Lock()
	defer t.wmu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	return t.ws.Write(ctx, typ, b)
}

func (t *wsTerm) Send(ctx context.Context, data []byte) error {
	return t.write(ctx, websocket.MessageBinary, data)
}

func (t *wsTerm) Resize(ctx context.Context, cols, rows int) error {
	b, _ := json.Marshal(fleetapi.TermFrame{Type: fleetapi.FrameResize, Cols: cols, Rows: rows})
	return t.write(ctx, websocket.MessageText, b)
}

func (t *wsTerm) Close() error {
	return t.ws.Close(websocket.StatusNormalClosure, "")
}
