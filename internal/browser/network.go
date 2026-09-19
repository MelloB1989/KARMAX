package browser

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"
)

// Wire-shaped types for the pieces of each CDP payload this package actually
// reads — hand-rolled rather than github.com/chromedp/cdproto's generated
// types. cdproto is already a (chromedp) transitive dependency and was the
// first route tried, per the task's suggestion, but its enums (ResourceType,
// ReferrerPolicy, IPAddressSpace, RemoteObject subtypes, ...) decode through
// a strict switch that errors outright on any value the vendored snapshot —
// 2022 — predates. A live, current browser sends values that snapshot does
// not know: Network.requestWillBeSentExtraInfo's own
// clientSecurityState.initiatorIPAddressSpace ("Loopback") failed exactly
// this way against a real headless Chrome 152 during this package's own
// tests (see network_test.go). Plain fields decoded with standard
// encoding/json cannot have that failure mode — an enum value the browser
// added after this file was written just comes back as an ordinary string —
// so capture stays resilient to whatever Chrome version the operator
// actually runs instead of being pinned to cdproto's vintage. The socket
// itself is still driven by hand over coder/websocket, in the package's
// existing style (cdp.go).

type wireRequestWillBeSent struct {
	RequestID string `json:"requestId"`
	Type      string `json:"type"`
	Request   struct {
		URL      string            `json:"url"`
		Method   string            `json:"method"`
		Headers  map[string]string `json:"headers"`
		PostData string            `json:"postData"`
	} `json:"request"`
}

type wireRequestExtraInfo struct {
	RequestID string            `json:"requestId"`
	Headers   map[string]string `json:"headers"`
}

type wireResponseReceived struct {
	RequestID string `json:"requestId"`
	Response  struct {
		Status   int               `json:"status"`
		Headers  map[string]string `json:"headers"`
		MimeType string            `json:"mimeType"`
	} `json:"response"`
}

// wireRequestID is every event this package only needs to correlate by id:
// loadingFinished and loadingFailed.
type wireRequestID struct {
	RequestID string `json:"requestId"`
}

type wireGetResponseBody struct {
	Body          string `json:"body"`
	Base64Encoded bool   `json:"base64Encoded"`
}

// wireEvaluateResult is Runtime.evaluate's return shape, trimmed to what
// Eval needs — the same reasoning as above applies to RemoteObject's own
// Type/Subtype enums, which a page's return value could exercise with
// anything.
type wireEvaluateResult struct {
	Result *struct {
		Value json.RawMessage `json:"value"`
	} `json:"result"`
	ExceptionDetails *struct {
		Text      string `json:"text"`
		Exception *struct {
			Description string `json:"description"`
		} `json:"exception"`
	} `json:"exceptionDetails"`
}

// Network capture: every request a page makes, kept in memory so a harness
// can discover and replay a site's own API instead of scripting a browser
// through it by hand. This is generic — nothing here knows about any
// particular site — which is the point: it replaces the bespoke
// site-specific scripts the removed skills package used to ship.
//
// One connection per page target, dialed at that page's own
// webSocketDebuggerUrl (see cdp.go), Network domain enabled on it. Requests
// land in a ring buffer shared across every tab, bounded by count AND by the
// bytes of response body actually retained — a page serving nothing but 1MB
// JSON blobs must not be able to grow this past a fixed budget.
const (
	maxCapturedRequests  = 1000
	maxCapturedBodyBytes = 1 << 20  // per-response cap; larger bodies are flagged, not buffered
	maxTotalBodyBytes    = 32 << 20 // ring-wide cap on retained body bytes

	networkDiscoveryInterval = 400 * time.Millisecond
)

// CapturedRequest is one request/response pair as observed on the wire.
// ResponseBody is empty whenever BodyOmitted is true — the reason says why.
type CapturedRequest struct {
	ID           string `json:"id"`
	TabID        string `json:"tab_id"`
	Method       string `json:"method"`
	URL          string `json:"url"`
	ResourceType string `json:"resource_type"` // xhr, fetch, document, other

	RequestHeaders map[string]string `json:"request_headers,omitempty"`
	RequestBody    string            `json:"request_body,omitempty"`

	Status          int               `json:"status,omitempty"`
	ResponseHeaders map[string]string `json:"response_headers,omitempty"`
	MimeType        string            `json:"mime_type,omitempty"`

	ResponseBody      string `json:"response_body,omitempty"`
	BodyOmitted       bool   `json:"body_omitted,omitempty"`
	BodyOmittedReason string `json:"body_omitted_reason,omitempty"` // "too large" or "binary"
	BodyBytes         int    `json:"body_bytes,omitempty"`          // size CDP reported, even when omitted

	Failed bool `json:"failed,omitempty"`

	StartedAt   time.Time `json:"started_at"`
	CompletedAt time.Time `json:"completed_at,omitempty"`
}

// RequestFilter narrows Requests. Zero values match everything.
type RequestFilter struct {
	TabID        string
	URLContains  string
	Method       string
	ResourceType string
	Status       int
	Limit        int
}

func (f RequestFilter) matches(r *CapturedRequest) bool {
	if f.TabID != "" && r.TabID != f.TabID {
		return false
	}
	if f.URLContains != "" && !strings.Contains(r.URL, f.URLContains) {
		return false
	}
	if f.Method != "" && !strings.EqualFold(r.Method, f.Method) {
		return false
	}
	if f.ResourceType != "" && !strings.EqualFold(r.ResourceType, f.ResourceType) {
		return false
	}
	if f.Status != 0 && r.Status != f.Status {
		return false
	}
	return true
}

// record is the store's own bookkeeping around a CapturedRequest — never
// handed to a caller, so it can hold what eviction and in-flight updates
// need without shaping the public type.
type record struct {
	data      CapturedRequest
	cdpKey    string // tabID + cdp's own requestId, while the request is in flight
	bodyBytes int64  // bytes counted toward the store's total, 0 once omitted
}

// requestStore is the ring buffer. Safe for concurrent use — every tab's
// capture goroutine writes into the same one.
type requestStore struct {
	mu       sync.Mutex
	capacity int
	maxBytes int64

	order      []*record // oldest first
	byID       map[string]*record
	byCDPKey   map[string]*record
	totalBytes int64
	counter    uint64

	// pendingExtraHeaders holds a requestWillBeSentExtraInfo's raw wire
	// headers when it arrives before the requestWillBeSent it belongs to —
	// CDP documents that ordering as unspecified. Entries live only for the
	// gap between the two events for one request, so this never grows past
	// however many requests are mid-flight at once.
	pendingExtraHeaders map[string]map[string]string
}

func newRequestStore(capacity int, maxBytes int64) *requestStore {
	return &requestStore{
		capacity:            capacity,
		maxBytes:            maxBytes,
		byID:                map[string]*record{},
		byCDPKey:            map[string]*record{},
		pendingExtraHeaders: map[string]map[string]string{},
	}
}

func cdpKey(tabID, cdpRequestID string) string { return tabID + "|" + cdpRequestID }

func (s *requestStore) begin(tabID, cdpRequestID string, data CapturedRequest) *record {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := cdpKey(tabID, cdpRequestID)
	if extra, ok := s.pendingExtraHeaders[key]; ok {
		data.RequestHeaders = mergeHeaders(data.RequestHeaders, extra)
		delete(s.pendingExtraHeaders, key)
	}
	s.counter++
	data.ID = fmt.Sprintf("r%d", s.counter)
	data.TabID = tabID
	rec := &record{data: data, cdpKey: key}
	s.order = append(s.order, rec)
	s.byID[rec.data.ID] = rec
	s.byCDPKey[rec.cdpKey] = rec
	s.evictLocked()
	return rec
}

func (s *requestStore) find(tabID, cdpRequestID string) (*record, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.byCDPKey[cdpKey(tabID, cdpRequestID)]
	return rec, ok
}

// applyExtraRequestHeaders merges the raw wire headers a
// requestWillBeSentExtraInfo event carries — including Cookie, which
// requestWillBeSent's own headers deliberately omit — into the matching
// record. Stashed for begin() to pick up when the extra-info event wins the
// race and arrives first.
func (s *requestStore) applyExtraRequestHeaders(tabID, cdpRequestID string, headers map[string]string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := cdpKey(tabID, cdpRequestID)
	if rec, ok := s.byCDPKey[key]; ok {
		rec.data.RequestHeaders = mergeHeaders(rec.data.RequestHeaders, headers)
		return
	}
	s.pendingExtraHeaders[key] = headers
}

func mergeHeaders(base, extra map[string]string) map[string]string {
	out := make(map[string]string, len(base)+len(extra))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range extra {
		out[k] = v
	}
	return out
}

func (s *requestStore) updateResponse(rec *record, status int, headers map[string]string, mime string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec.data.Status = status
	rec.data.ResponseHeaders = headers
	rec.data.MimeType = mime
}

// updateBody records the fetched body (or the reason it was not kept) and
// retires the record from byCDPKey — nothing else updates it after this.
func (s *requestStore) updateBody(rec *record, body string, omitted bool, reason string, bytes int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if omitted {
		rec.data.BodyOmitted = true
		rec.data.BodyOmittedReason = reason
	} else {
		rec.data.ResponseBody = body
		rec.bodyBytes = int64(len(body))
		s.totalBytes += rec.bodyBytes
	}
	rec.data.BodyBytes = bytes
	rec.data.CompletedAt = time.Now()
	delete(s.byCDPKey, rec.cdpKey)
	s.evictLocked()
}

func (s *requestStore) markFailed(rec *record) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec.data.Failed = true
	rec.data.CompletedAt = time.Now()
	delete(s.byCDPKey, rec.cdpKey)
}

// evictLocked drops the oldest records until both the count and the retained
// body bytes are back under budget. Called with mu held.
func (s *requestStore) evictLocked() {
	for len(s.order) > 0 && (len(s.order) > s.capacity || s.totalBytes > s.maxBytes) {
		oldest := s.order[0]
		s.order = s.order[1:]
		delete(s.byID, oldest.data.ID)
		delete(s.byCDPKey, oldest.cdpKey)
		s.totalBytes -= oldest.bodyBytes
	}
}

func (s *requestStore) list(filter RequestFilter) []CapturedRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]CapturedRequest, 0, len(s.order))
	for i := len(s.order) - 1; i >= 0; i-- { // newest first
		if rec := s.order[i]; filter.matches(&rec.data) {
			out = append(out, rec.data)
		}
	}
	if filter.Limit > 0 && len(out) > filter.Limit {
		out = out[:filter.Limit]
	}
	return out
}

func (s *requestStore) get(id string) (CapturedRequest, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.byID[id]
	if !ok {
		return CapturedRequest{}, false
	}
	return rec.data, true
}

// Requests returns captured requests matching filter, newest first. filter's
// zero value matches everything, bounded only by the ring's own capacity.
func (s *Session) Requests(filter RequestFilter) []CapturedRequest {
	return s.netStore.list(filter)
}

// RequestByID returns one captured request in full, including whatever body
// was retained.
func (s *Session) RequestByID(id string) (CapturedRequest, bool) {
	return s.netStore.get(id)
}

// tabCapture is what startNetworkCapture keeps per attached tab.
type tabCapture struct {
	conn   *cdpConn
	cancel func()
}

// startNetworkCapture attaches to every open page target and keeps attaching
// to new ones, until stopNetworkCapture is called. Idempotent: a second call
// while already running does nothing.
func (s *Session) startNetworkCapture() {
	s.netMu.Lock()
	if s.netCancel != nil {
		s.netMu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.netCancel = cancel
	s.netCtx = ctx
	s.attached = map[string]*tabCapture{}
	s.netMu.Unlock()

	s.netWG.Add(1)
	go s.networkDiscoveryLoop(ctx)
}

// stopNetworkCapture detaches from every tab and waits for its goroutines to
// actually exit, so Stop() never returns with a capture connection still
// live against a browser that is on its way down.
func (s *Session) stopNetworkCapture() {
	s.netMu.Lock()
	cancel := s.netCancel
	s.netCancel = nil
	s.netCtx = nil
	s.netMu.Unlock()
	if cancel == nil {
		return
	}
	cancel()
	s.netWG.Wait()
}

func (s *Session) networkDiscoveryLoop(ctx context.Context) {
	defer s.netWG.Done()
	s.syncNetworkTargets(ctx)
	t := time.NewTicker(networkDiscoveryInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			s.detachAllTabs()
			return
		case <-t.C:
			s.syncNetworkTargets(ctx)
		}
	}
}

// syncNetworkTargets attaches to any page target not already attached and
// detaches from any that closed. Runs only from the discovery loop's own
// goroutine — attachTab's blocking dial-and-enable therefore never races a
// second attempt at the same tab.
func (s *Session) syncNetworkTargets(ctx context.Context) {
	endpoint := s.Endpoint(ctx)
	if endpoint == "" {
		return
	}
	var all []Tab
	if err := getJSON(ctx, endpoint+"/json/list", &all); err != nil {
		return
	}

	seen := make(map[string]bool, len(all))
	for _, tab := range all {
		if tab.Type != "page" || tab.WebSocketDebuggerURL == "" {
			continue
		}
		seen[tab.ID] = true

		s.netMu.Lock()
		_, ok := s.attached[tab.ID]
		s.netMu.Unlock()
		if ok {
			continue
		}
		if ctx.Err() != nil {
			return
		}
		s.attachTab(ctx, tab.ID, tab.WebSocketDebuggerURL)
	}

	s.netMu.Lock()
	var stale []*tabCapture
	for id, tc := range s.attached {
		if !seen[id] {
			stale = append(stale, tc)
			delete(s.attached, id)
		}
	}
	s.netMu.Unlock()
	for _, tc := range stale {
		tc.cancel()
	}
}

// attachTabIfNeeded returns tabID's persistent capture connection,
// attaching one first when none exists yet: dialing the tab's own DevTools
// endpoint, enabling Network, and waiting for that command's own reply
// before returning — so a caller that goes on to navigate the tab over the
// returned connection knows capture is already live for whatever the
// navigation is about to fire. Safe to call concurrently with the
// discovery loop (syncNetworkTargets) or another attach for the same tab —
// losing the race just means the connection just dialed is closed unused
// and the winner's is returned instead; nothing double-attaches.
//
// The returned connection's own lifetime is rooted in the session's
// long-lived capture context (s.netCtx), never callerCtx: callerCtx only
// bounds the dial and the Network.enable round trip, and must not be able
// to tear down ongoing capture just because the call that attached it — an
// Open(), say — returned. The read loop is registered with netWG before
// the (possibly slow) enable call, so stopNetworkCapture's Wait always
// accounts for it, including the case where cancellation lands mid-enable
// and this attach is abandoned.
func (s *Session) attachTabIfNeeded(callerCtx context.Context, tabID, wsURL string) (*tabCapture, error) {
	s.netMu.Lock()
	if tc, ok := s.attached[tabID]; ok {
		s.netMu.Unlock()
		return tc, nil
	}
	root := s.netCtx
	s.netMu.Unlock()
	if root == nil {
		// Defensive only: every path that can reach here calls Start()
		// first, which always calls startNetworkCapture — so netCtx
		// should never actually be nil here.
		root = callerCtx
	}

	conn, err := dialCDP(callerCtx, wsURL)
	if err != nil {
		return nil, err
	}
	tabCtx, cancel := context.WithCancel(root)
	conn.onEvent = func(method string, params json.RawMessage) {
		s.handleNetworkEvent(tabID, conn, method, params)
	}

	s.netWG.Add(1)
	go func() {
		defer s.netWG.Done()
		conn.readLoop(tabCtx)
	}()

	if err := conn.call(callerCtx, "Network.enable", struct{}{}, nil); err != nil {
		cancel()
		conn.Close()
		return nil, err
	}

	tc := &tabCapture{conn: conn, cancel: func() { cancel(); conn.Close() }}
	s.netMu.Lock()
	if existing, ok := s.attached[tabID]; ok {
		// Lost the race — the discovery loop, or a concurrent Open(),
		// attached this tab first. Keep theirs, drop what was just dialed.
		s.netMu.Unlock()
		tc.cancel()
		return existing, nil
	}
	s.attached[tabID] = tc
	s.netMu.Unlock()
	return tc, nil
}

// attachTab is attachTabIfNeeded for the discovery loop, which only needs
// the attach to happen and has no use for the connection itself.
func (s *Session) attachTab(parent context.Context, tabID, wsURL string) {
	_, _ = s.attachTabIfNeeded(parent, tabID, wsURL)
}

func (s *Session) detachAllTabs() {
	s.netMu.Lock()
	tcs := make([]*tabCapture, 0, len(s.attached))
	for id, tc := range s.attached {
		tcs = append(tcs, tc)
		delete(s.attached, id)
	}
	s.netMu.Unlock()
	for _, tc := range tcs {
		tc.cancel()
	}
}

func (s *Session) handleNetworkEvent(tabID string, conn *cdpConn, method string, params json.RawMessage) {
	switch method {
	case "Network.requestWillBeSent":
		var ev wireRequestWillBeSent
		if err := json.Unmarshal(params, &ev); err != nil || ev.RequestID == "" {
			return
		}
		s.netStore.begin(tabID, ev.RequestID, CapturedRequest{
			Method:         ev.Request.Method,
			URL:            ev.Request.URL,
			ResourceType:   classifyResourceType(ev.Type),
			RequestHeaders: ev.Request.Headers,
			RequestBody:    ev.Request.PostData,
			StartedAt:      time.Now(),
		})

	// requestWillBeSent's own headers deliberately omit Cookie (and a few
	// other browser-controlled ones); requestWillBeSentExtraInfo carries the
	// raw headers actually sent over the wire, Cookie included.
	case "Network.requestWillBeSentExtraInfo":
		var ev wireRequestExtraInfo
		if err := json.Unmarshal(params, &ev); err != nil || len(ev.Headers) == 0 {
			return
		}
		s.netStore.applyExtraRequestHeaders(tabID, ev.RequestID, ev.Headers)

	case "Network.responseReceived":
		var ev wireResponseReceived
		if err := json.Unmarshal(params, &ev); err != nil {
			return
		}
		rec, ok := s.netStore.find(tabID, ev.RequestID)
		if !ok {
			return
		}
		s.netStore.updateResponse(rec, ev.Response.Status, ev.Response.Headers, ev.Response.MimeType)

	case "Network.loadingFinished":
		var ev wireRequestID
		if err := json.Unmarshal(params, &ev); err != nil {
			return
		}
		rec, ok := s.netStore.find(tabID, ev.RequestID)
		if !ok {
			return
		}
		go s.fetchResponseBody(conn, rec, ev.RequestID)

	case "Network.loadingFailed":
		var ev wireRequestID
		if err := json.Unmarshal(params, &ev); err != nil {
			return
		}
		if rec, ok := s.netStore.find(tabID, ev.RequestID); ok {
			s.netStore.markFailed(rec)
		}
	}
}

// fetchResponseBody runs off the read loop (see onEvent's contract) since
// getResponseBody is itself a call over the same connection that delivered
// loadingFinished.
func (s *Session) fetchResponseBody(conn *cdpConn, rec *record, reqID string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var ret wireGetResponseBody
	if err := conn.call(ctx, "Network.getResponseBody", map[string]string{"requestId": reqID}, &ret); err != nil {
		s.netStore.updateBody(rec, "", true, "unavailable", 0)
		return
	}
	if ret.Base64Encoded {
		s.netStore.updateBody(rec, "", true, "binary", len(ret.Body))
		return
	}
	if len(ret.Body) > maxCapturedBodyBytes {
		s.netStore.updateBody(rec, "", true, "too large", len(ret.Body))
		return
	}
	s.netStore.updateBody(rec, ret.Body, false, "", len(ret.Body))
}

func classifyResourceType(t string) string {
	switch strings.ToLower(t) {
	case "xhr":
		return "xhr"
	case "fetch":
		return "fetch"
	case "document":
		return "document"
	default:
		return "other"
	}
}

// replayHeaderBlocklist are headers a page's own fetch() may not set (the
// browser owns them) and that a captured request carries but a replay must
// not try to force: Cookie is attached from the tab's own jar, Host and
// Content-Length are computed by the browser for the actual request it sends.
var replayHeaderBlocklist = map[string]bool{"cookie": true, "host": true, "content-length": true}

// FilterReplayHeaders drops the headers a browser fetch() call is not
// allowed to set by hand. Used both by FetchInTab itself and by callers
// building a FetchSpec from a captured request (the CLI's --from).
func FilterReplayHeaders(h map[string]string) map[string]string {
	out := make(map[string]string, len(h))
	for k, v := range h {
		if !replayHeaderBlocklist[strings.ToLower(k)] {
			out[k] = v
		}
	}
	return out
}

// FetchSpec is a request to replay from inside a tab.
type FetchSpec struct {
	URL     string
	Method  string
	Headers map[string]string
	Body    string
	HasBody bool // distinguishes "no body" from "an explicit empty body"
}

// FetchResult is what the replayed fetch() actually got back.
type FetchResult struct {
	Status  int
	Headers map[string]string
	Body    string
}

// FetchInTab runs fetch() inside tabID via Runtime.evaluate, so the tab's own
// cookies and any same-site session state apply exactly as they would for a
// request the page made itself. This is the replay primitive: given a
// captured request's method/headers/body, it reproduces the call as that
// tab, not as this process.
func (s *Session) FetchInTab(ctx context.Context, tabID string, spec FetchSpec) (FetchResult, error) {
	if strings.TrimSpace(spec.URL) == "" {
		return FetchResult{}, fmt.Errorf("fetch in tab: no URL")
	}
	method := strings.TrimSpace(spec.Method)
	if method == "" {
		method = "GET"
	}
	headers := FilterReplayHeaders(spec.Headers)

	raw, err := s.Eval(ctx, tabID, buildFetchExpr(spec.URL, method, headers, spec.Body, spec.HasBody))
	if err != nil {
		return FetchResult{}, err
	}
	var out struct {
		Status  int               `json:"status"`
		Headers map[string]string `json:"headers"`
		Body    string            `json:"body"`
		Error   string            `json:"error"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return FetchResult{}, fmt.Errorf("fetch in tab: decoding result: %w", err)
	}
	if out.Error != "" {
		return FetchResult{}, fmt.Errorf("fetch in tab: %s", out.Error)
	}
	return FetchResult{Status: out.Status, Headers: out.Headers, Body: out.Body}, nil
}

// buildFetchExpr assembles a self-contained async expression: run fetch,
// await it, read the body as text, and return a plain JSON-shaped object —
// or {error} if the fetch itself rejected (CORS, DNS, a closed connection).
// Every dynamic piece goes through json.Marshal so it lands as a JS literal,
// not source the caller's own URL/headers/body could break out of.
func buildFetchExpr(url, method string, headers map[string]string, body string, hasBody bool) string {
	urlJSON, _ := json.Marshal(url)
	methodJSON, _ := json.Marshal(method)
	headersJSON, _ := json.Marshal(headers)
	bodyExpr := "undefined"
	if hasBody {
		b, _ := json.Marshal(body)
		bodyExpr = string(b)
	}
	return fmt.Sprintf(`(async () => {
  try {
    const res = await fetch(%s, { method: %s, headers: %s, body: %s, credentials: 'include' });
    const text = await res.text();
    const headers = {};
    res.headers.forEach((v, k) => { headers[k] = v; });
    return { status: res.status, headers, body: text };
  } catch (e) {
    return { error: String((e && e.message) || e) };
  }
})()`, urlJSON, methodJSON, headersJSON, bodyExpr)
}

// Eval evaluates expr in tabID, awaiting a returned promise, and gives back
// the result serialized as JSON — whatever the expression's final value
// JSON-encodes to. Used directly by the eval tool/CLI action, and as
// FetchInTab's own primitive.
func (s *Session) Eval(ctx context.Context, tabID, expr string) (json.RawMessage, error) {
	conn, cleanup, err := s.connForTab(ctx, tabID)
	if err != nil {
		return nil, err
	}
	defer cleanup()

	var ret wireEvaluateResult
	params := map[string]any{"expression": expr, "awaitPromise": true, "returnByValue": true}
	if err := conn.call(ctx, "Runtime.evaluate", params, &ret); err != nil {
		return nil, fmt.Errorf("eval: %w", err)
	}
	if ret.ExceptionDetails != nil {
		msg := ret.ExceptionDetails.Text
		if ret.ExceptionDetails.Exception != nil && ret.ExceptionDetails.Exception.Description != "" {
			msg = ret.ExceptionDetails.Exception.Description
		}
		return nil, fmt.Errorf("eval: %s", msg)
	}
	if ret.Result == nil || len(ret.Result.Value) == 0 {
		return json.RawMessage("null"), nil
	}
	return ret.Result.Value, nil
}

// connForTab returns the tab's persistent capture connection when one is
// already attached, or dials a one-off connection when the discovery loop
// has not reached it yet (a tab that just opened, for instance) — so a
// caller never has to wait out a poll tick just to run one eval. cleanup is
// a no-op for the persistent connection and closes the one-off dial.
func (s *Session) connForTab(ctx context.Context, tabID string) (*cdpConn, func(), error) {
	s.netMu.Lock()
	tc, ok := s.attached[tabID]
	s.netMu.Unlock()
	if ok {
		return tc.conn, func() {}, nil
	}

	endpoint := s.Endpoint(ctx)
	if endpoint == "" {
		return nil, nil, ErrNotRunning
	}
	var all []Tab
	if err := getJSON(ctx, endpoint+"/json/list", &all); err != nil {
		return nil, nil, err
	}
	for _, tab := range all {
		if tab.ID == tabID && tab.WebSocketDebuggerURL != "" {
			conn, err := dialCDP(ctx, tab.WebSocketDebuggerURL)
			if err != nil {
				return nil, nil, err
			}
			go conn.readLoop(ctx)
			return conn, func() { conn.Close() }, nil
		}
	}
	return nil, nil, fmt.Errorf("browser: no such tab %q", tabID)
}
