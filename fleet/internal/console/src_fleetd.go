package console

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// FleetdClient is the console's door to fleetd, with the console-scope
// token read from its file on every request (so a rotated token is picked
// up without a restart). The token is never logged or put in an error.
type FleetdClient struct {
	Base      string
	TokenFile string
	HTTP      *http.Client
}

func NewFleetdClient(base, tokenFile string) *FleetdClient {
	return &FleetdClient{Base: strings.TrimRight(base, "/"), TokenFile: tokenFile, HTTP: &http.Client{Timeout: 10 * time.Second}}
}

func (c *FleetdClient) token() (string, error) {
	b, err := os.ReadFile(c.TokenFile)
	if err != nil {
		return "", fmt.Errorf("fleetd token: %w", err)
	}
	return strings.TrimSpace(string(b)), nil
}

// Do sends one request and returns the status and body verbatim.
func (c *FleetdClient) Do(ctx context.Context, method, path string, body []byte) (int, []byte, error) {
	tok, err := c.token()
	if err != nil {
		return 0, nil, err
	}
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.Base+path, rd)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("fleetd %s %s: %w", method, strings.SplitN(path, "?", 2)[0], urlErr(err))
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	return resp.StatusCode, b, err
}

// urlErr drops the URL from a transport error; the message is shown on the
// panel.
func urlErr(err error) error {
	if ue, ok := err.(*url.Error); ok {
		return ue.Err
	}
	return err
}

func (c *FleetdClient) get(ctx context.Context, path string, out any) error {
	code, b, err := c.Do(ctx, http.MethodGet, path, nil)
	if err != nil {
		return err
	}
	if code != http.StatusOK {
		msg := strings.TrimSpace(string(b))
		if len(msg) > 200 {
			msg = msg[:200] + "…"
		}
		return fmt.Errorf("fleetd GET %s: %d %s", strings.SplitN(path, "?", 2)[0], code, msg)
	}
	if err := json.Unmarshal(b, out); err != nil {
		return fmt.Errorf("fleetd GET %s: %w", strings.SplitN(path, "?", 2)[0], err)
	}
	return nil
}

// Wire shapes: fleetd's JSON, as agreed with Task 4A.

type fleetdRow struct {
	Agent      string    `json:"agent"`
	Host       string    `json:"host"`
	State      string    `json:"state"`
	Task       string    `json:"task"`
	IdleS      int64     `json:"idle_s"`
	Model      string    `json:"model"`
	Alerts     []string  `json:"alerts"`
	Models     []string  `json:"models"`
	Session    string    `json:"session"`
	RecordedAt time.Time `json:"recorded_at"`
}

type fleetdStatus struct {
	Agents       []fleetdRow `json:"agents"`
	Orchestrator *struct {
		FiveHour       float64   `json:"FiveHour"`
		SevenDay       float64   `json:"SevenDay"`
		FiveHourResets time.Time `json:"FiveHourResets"`
		SevenDayResets time.Time `json:"SevenDayResets"`
	} `json:"orchestrator"`
}

type fleetdTask struct {
	ID       string    `json:"id"`
	Agent    string    `json:"agent"`
	Assigner string    `json:"assigner"`
	Assigned time.Time `json:"assigned"`
	Closed   time.Time `json:"closed"`
	Outcome  string    `json:"outcome"`
}

type fleetdEvent struct {
	ID      int64           `json:"id"`
	Agent   string          `json:"agent"`
	At      time.Time       `json:"at"`
	Kind    string          `json:"kind"`
	Session string          `json:"session"`
	Detail  json.RawMessage `json:"detail"`
}

type fleetdActivity struct {
	BucketS int                 `json:"bucket_s"`
	Agents  map[string][]Bucket `json:"agents"`
}

type fleetdToday struct {
	Since        time.Time `json:"since"`
	TasksClosed  int       `json:"tasks_closed"`
	SpendUSD     float64   `json:"spend_usd"`
	OutputTokens int64     `json:"output_tokens"`
}

// A session as /v1/history returns it: ledger.Session has no json tags, so
// Go's field names; the snake_case spelling is accepted too.
type fleetdSession struct {
	Task    string  `json:"Task"`
	CostUSD float64 `json:"CostUSD"`
	Cost    float64 `json:"cost_usd"`
	Task2   string  `json:"task"`
}

func (s fleetdSession) task() string {
	if s.Task != "" {
		return s.Task
	}
	return s.Task2
}
func (s fleetdSession) cost() float64 {
	if s.CostUSD != 0 {
		return s.CostUSD
	}
	return s.Cost
}

// kindsParam is the stream kinds, as fleetd's `kinds` filter takes them.
func kindsParam() string {
	return strings.Join(StreamKinds, ",")
}

// fleetdSrc polls fleetd and keeps the last good value of each answer. When
// fleetd is down the values stay, with their old `recorded_at`, and the
// source says it is stale.
type fleetdSrc struct {
	cl  *FleetdClient
	now func() time.Time
	tr  *tracker

	mu       sync.Mutex
	rows     map[string]fleetdRow
	orch     *Orchestrator
	tasks    []Task
	events   []Event // recorded, ascending by id, the most recent liveEventsCap
	cursor   int64
	activity map[string][]Bucket
	today    Today
	lastTask map[string]*LastTask
	polledAt time.Time // the last good status poll
	orchName string
	agents   []string
}

func newFleetdSrc(cl *FleetdClient, now func() time.Time, orchName string, agents []string) *fleetdSrc {
	return &fleetdSrc{cl: cl, now: now, tr: newTracker(CadenceFleetd, false), rows: map[string]fleetdRow{},
		activity: map[string][]Bucket{}, lastTask: map[string]*LastTask{}, orchName: orchName, agents: agents}
}

// poll is one 5 s round: status, open tasks, new events. Any failure marks
// the source failed; the values stay as they were.
func (f *fleetdSrc) poll(ctx context.Context) error {
	err := f.pollOnce(ctx)
	if err != nil {
		f.tr.failure(err)
		return err
	}
	f.tr.success(f.now())
	return nil
}

func (f *fleetdSrc) pollOnce(ctx context.Context) error {
	var st fleetdStatus
	if err := f.cl.get(ctx, "/v1/status", &st); err != nil {
		return err
	}
	var tasks []fleetdTask
	if err := f.cl.get(ctx, "/v1/tasks?open=1", &tasks); err != nil {
		return err
	}
	evs, err := f.newEvents(ctx)
	if err != nil {
		return err
	}
	now := f.now()
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rows = map[string]fleetdRow{}
	for _, r := range st.Agents {
		f.rows[r.Agent] = r
	}
	f.orch = nil
	if o := st.Orchestrator; o != nil {
		f.orch = &Orchestrator{Name: f.orchName, FiveHourPct: o.FiveHour, SevenDayPct: o.SevenDay,
			FiveHourResets: o.FiveHourResets, SevenDayResets: o.SevenDayResets, At: now}
	}
	f.tasks = f.tasks[:0:0]
	for _, t := range tasks {
		f.tasks = append(f.tasks, Task{ID: t.ID, Agent: t.Agent, Assigner: t.Assigner, Since: t.Assigned})
	}
	f.events = append(f.events, evs...)
	if len(f.events) > liveEventsCap {
		f.events = append([]Event(nil), f.events[len(f.events)-liveEventsCap:]...)
	}
	f.polledAt = now
	return nil
}

const (
	eventsPage     = 500
	eventsMaxPages = 20
)

// newEvents reads the stream events after the cursor, ascending by id. The
// first call pages from the beginning and keeps the tail (the route is a
// cursor, so the newest 200 are only known after reading to the end).
func (f *fleetdSrc) newEvents(ctx context.Context) ([]Event, error) {
	f.mu.Lock()
	after := f.cursor
	f.mu.Unlock()
	var out []Event
	for range eventsMaxPages {
		var page []fleetdEvent
		q := fmt.Sprintf("/v1/events?after=%d&kinds=%s&limit=%d", after, url.QueryEscape(kindsParam()), eventsPage)
		if err := f.cl.get(ctx, q, &page); err != nil {
			return nil, err
		}
		for _, e := range page {
			out = append(out, Event{ID: e.ID, At: e.At, Agent: e.Agent, Kind: e.Kind, Session: e.Session, Detail: e.Detail})
			after = max(after, e.ID)
		}
		if len(page) < eventsPage {
			break
		}
		if len(out) > 4*liveEventsCap {
			out = out[len(out)-liveEventsCap:]
		}
	}
	f.mu.Lock()
	f.cursor = after
	f.mu.Unlock()
	return out, nil
}

// pollActivity is the 60 s round. Its failure leaves the old buckets.
func (f *fleetdSrc) pollActivity(ctx context.Context) error {
	var a fleetdActivity
	if err := f.cl.get(ctx, "/v1/activity?since=24h&bucket=15m", &a); err != nil {
		return err
	}
	f.mu.Lock()
	f.activity = a.Agents
	f.mu.Unlock()
	return nil
}

// pollToday is the 30 s round: today's totals and each agent's last task.
func (f *fleetdSrc) pollToday(ctx context.Context) error {
	var td fleetdToday
	if err := f.cl.get(ctx, "/v1/today", &td); err != nil {
		return err
	}
	last := map[string]*LastTask{}
	for _, name := range f.agents {
		var ts []fleetdTask
		if err := f.cl.get(ctx, "/v1/tasks?agent="+url.QueryEscape(name)+"&n=1", &ts); err != nil {
			return err
		}
		if len(ts) == 0 || ts[0].Closed.IsZero() {
			continue // never had a task, or its newest is still open
		}
		lt := &LastTask{ID: ts[0].ID, Outcome: ts[0].Outcome, Ended: ts[0].Closed}
		var ss []fleetdSession
		if err := f.cl.get(ctx, "/v1/history?agent="+url.QueryEscape(name)+"&n="+strconv.Itoa(20), &ss); err == nil {
			for _, s := range ss {
				if s.task() == lt.ID {
					lt.CostUSD += s.cost()
				}
			}
		}
		last[name] = lt
	}
	f.mu.Lock()
	f.today = Today{Since: td.Since, TasksClosed: td.TasksClosed, SpendUSD: td.SpendUSD, OutputTokens: td.OutputTokens}
	f.lastTask = last
	f.mu.Unlock()
	return nil
}

func (f *fleetdSrc) run(ctx context.Context, changed func()) {
	var wg sync.WaitGroup
	wg.Add(3)
	go func() {
		defer wg.Done()
		every(ctx, CadenceFleetd, func(ctx context.Context) { f.poll(ctx); changed() })
	}()
	go func() {
		defer wg.Done()
		every(ctx, cadenceActivity, func(ctx context.Context) { f.pollActivity(ctx); changed() })
	}()
	go func() {
		defer wg.Done()
		every(ctx, cadenceToday, func(ctx context.Context) { f.pollToday(ctx); changed() })
	}()
	wg.Wait()
}
