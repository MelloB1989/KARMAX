// Package posthog is the PostHog event source: errors that are new or
// spiking, insight alerts that go off, and — once a day — a key metric that
// moved far from its week, for every project a personal API key can see.
//
// Exceptions are read with HogQL over the $exception events rather than
// through the error-tracking endpoints, because the query API is the stable
// public surface and counts per issue are exactly what "spiking" needs:
// the last Window set beside the Baseline before it. Alerts are read from
// the alerts API, and each alert's last state is kept in the store, so a
// firing alert is told once per time it goes off rather than once per poll.
package posthog

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/MelloB1989/karmax/internal/sources"
)

// Name is the event source's name and route prefix.
const Name = "posthog"

// Account is one PostHog login: a personal API key and where it points.
type Account struct {
	// Integration is the KARMAX integration it came from ("posthog", or
	// "posthog:work" for a second login), which `karmax login` is run with.
	Integration string
	APIKey      string // phx_…
	Host        string // the app host; the capture host is mapped to it
	// ProjectIDs to watch; empty watches every project the key can list.
	ProjectIDs []string
}

// Config tunes the source. Zero values take the defaults below.
type Config struct {
	ErrorsEvery   time.Duration // how often exceptions are counted (default 15m)
	Window        time.Duration // "recent" for an error (default 1h)
	Baseline      time.Duration // what recent is set beside (default 7 days)
	SpikeFactor   float64       // recent ≥ this × the usual rate… (default 4)
	SpikeMin      int           // …and at least this many (default 20)
	SpikeCooldown time.Duration // one spike per issue per this long (default 6h)

	AnomalyOff    bool     // turns the daily metric check off
	Metrics       []string // event names to check; empty takes the busiest
	AnomalyTop    int      // how many busiest events (default 5)
	AnomalyChange float64  // a move of this fraction of the usual day (default 0.5)
	AnomalyMin    int      // events a usual day must have to be judged (default 50)
}

func (c Config) withDefaults() Config {
	if c.ErrorsEvery <= 0 {
		c.ErrorsEvery = 15 * time.Minute
	}
	if c.Window <= 0 {
		c.Window = time.Hour
	}
	if c.Baseline <= c.Window {
		c.Baseline = 7 * 24 * time.Hour
	}
	if c.SpikeFactor <= 0 {
		c.SpikeFactor = 4
	}
	if c.SpikeMin <= 0 {
		c.SpikeMin = 20
	}
	if c.SpikeCooldown <= 0 {
		c.SpikeCooldown = 6 * time.Hour
	}
	if c.AnomalyTop <= 0 {
		c.AnomalyTop = 5
	}
	if c.AnomalyChange <= 0 {
		c.AnomalyChange = 0.5
	}
	if c.AnomalyMin <= 0 {
		c.AnomalyMin = 50
	}
	return c
}

// KV is the store's key-value table (store.Store satisfies it).
type KV interface {
	KVGet(group, key string) (string, bool, error)
	KVSet(group, key, value string, ttl time.Duration) error
}

const (
	alertStateGroup = "posthog-alert-state"
	anomalyDayGroup = "posthog-anomaly-day"
)

// Scopes the personal API key needs.
const Scopes = "project:read, query:read, alert:read"

// OffReason says what to run so PostHog's events turn on.
func OffReason(integration string) string {
	if integration == "" {
		integration = "posthog"
	}
	return fmt.Sprintf("PostHog events are off: no personal API key. To turn them on: in PostHog open "+
		"Settings → Personal API keys, create a key with the scopes %s, then run `karmax login %s` and paste it "+
		"as posthog_api_key (posthog_host is https://us.posthog.com or https://eu.posthog.com or your own; "+
		"posthog_project_ids is optional, comma-separated, and empty watches every project). "+
		"POSTHOG_API_KEY, POSTHOG_HOST and POSTHOG_PROJECT_IDS in the daemon's environment work too. "+
		"The next poll picks it up; no restart is needed.", Scopes, integration)
}

// Source polls PostHog projects.
type Source struct {
	HTTP *http.Client
	Now  func() time.Time

	cfg       Config
	kv        KV
	publish   func(sources.Event) error
	seenNew   sources.Seen
	seenSpike sources.Seen
	seenOnce  sources.Seen

	tickMu sync.Mutex
	mu     sync.Mutex
	state  map[string]*accountState // by integration + host
}

type accountState struct {
	projects    []project
	listedAt    time.Time
	errorsAt    map[string]time.Time // project id -> last exception count
	pausedUntil time.Time
	lastOK      time.Time
	lastErr     string
}

type project struct {
	ID, Name string
}

// New builds the source. publish delivers each new event once.
func New(cfg Config, kv KV, publish func(sources.Event) error) *Source {
	cfg = cfg.withDefaults()
	seen := func(group string, ttl time.Duration) sources.Seen {
		return sources.Seen{Group: group, TTL: ttl, Get: kv.KVGet, Set: kv.KVSet}
	}
	return &Source{
		HTTP:      &http.Client{Timeout: 60 * time.Second},
		Now:       time.Now,
		cfg:       cfg,
		kv:        kv,
		publish:   publish,
		seenNew:   seen("posthog-seen-new", 30*24*time.Hour),
		seenSpike: seen("posthog-seen-spike", cfg.SpikeCooldown),
		seenOnce:  seen("posthog-seen", 30*24*time.Hour),
		state:     map[string]*accountState{},
	}
}

// Result is what one tick did.
type Result struct {
	Projects  int
	Published int
	// Off, when no account could be read, says why and what to run.
	Off string
}

// Status is one account's state, for health.
type Status struct {
	Integration string
	Projects    []string
	LastOK      time.Time
	LastError   string
	PausedUntil time.Time
}

// APIHost maps what an operator may paste — nothing, a bare host, or the
// capture host from their snippet — to the host the private API is on.
func APIHost(h string) string {
	h = strings.TrimRight(strings.TrimSpace(h), "/")
	if h == "" {
		return "https://us.posthog.com"
	}
	if !strings.Contains(h, "://") {
		h = "https://" + h
	}
	if u, err := url.Parse(h); err == nil {
		switch strings.ToLower(u.Host) {
		case "us.i.posthog.com", "app.posthog.com":
			return "https://us.posthog.com"
		case "eu.i.posthog.com":
			return "https://eu.posthog.com"
		}
	}
	return h
}

// Tick checks every account's projects once.
func (s *Source) Tick(ctx context.Context, accounts []Account) (Result, error) {
	s.tickMu.Lock()
	defer s.tickMu.Unlock()

	var res Result
	var good []Account
	var off []string
	for _, a := range accounts {
		if a.APIKey = strings.TrimSpace(a.APIKey); a.APIKey == "" {
			off = append(off, OffReason(a.Integration))
			continue
		}
		a.Host = APIHost(a.Host)
		good = append(good, a)
	}
	if len(good) == 0 {
		if len(off) == 0 {
			off = append(off, OffReason(""))
		}
		res.Off = strings.Join(off, "\n")
		return res, nil
	}

	var errs []string
	for _, a := range good {
		st := s.stateFor(a)
		now := s.Now()
		if now.Before(st.pausedUntil) {
			continue
		}
		err := s.checkAccount(ctx, a, st, &res)
		var rl *rateLimited
		s.mu.Lock()
		switch {
		case errors.As(err, &rl):
			st.pausedUntil = now.Add(rl.wait)
		case err != nil:
			st.lastErr = err.Error()
			errs = append(errs, fmt.Sprintf("%s: %v", label(a), err))
		default:
			st.lastErr, st.lastOK = "", now
		}
		s.mu.Unlock()
	}
	if len(errs) > 0 {
		return res, errors.New(strings.Join(errs, "; "))
	}
	return res, nil
}

func label(a Account) string {
	if a.Integration != "" {
		return a.Integration
	}
	return Name
}

func (s *Source) stateFor(a Account) *accountState {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := a.Integration + "|" + a.Host
	st, ok := s.state[k]
	if !ok {
		st = &accountState{errorsAt: map[string]time.Time{}}
		s.state[k] = st
	}
	return st
}

func (s *Source) checkAccount(ctx context.Context, a Account, st *accountState, res *Result) error {
	now := s.Now()
	if st.listedAt.IsZero() || now.Sub(st.listedAt) >= 6*time.Hour {
		ps, err := s.projects(ctx, a)
		if err != nil {
			return err
		}
		s.mu.Lock()
		st.projects, st.listedAt = ps, now
		s.mu.Unlock()
	}
	for _, p := range st.projects {
		res.Projects++
		if last, ok := st.errorsAt[p.ID]; !ok || now.Sub(last) >= s.cfg.ErrorsEvery {
			if err := s.checkErrors(ctx, a, p, res); err != nil {
				return err
			}
			st.errorsAt[p.ID] = now
		}
		if err := s.checkAlerts(ctx, a, p, res); err != nil {
			return err
		}
		if !s.cfg.AnomalyOff {
			if err := s.checkAnomalies(ctx, a, p, res); err != nil {
				return err
			}
		}
	}
	return nil
}

// projects names the projects to watch: the configured ids, or every one
// the key can list.
func (s *Source) projects(ctx context.Context, a Account) ([]project, error) {
	var out []project
	if len(a.ProjectIDs) > 0 {
		for _, id := range a.ProjectIDs {
			if id = strings.TrimSpace(id); id == "" {
				continue
			}
			var p struct {
				ID   json.Number `json:"id"`
				Name string      `json:"name"`
			}
			if err := s.do(ctx, a, http.MethodGet, "/api/projects/"+url.PathEscape(id)+"/", nil, &p); err != nil {
				return nil, err
			}
			out = append(out, project{ID: id, Name: firstNonEmpty(p.Name, id)})
		}
		return out, nil
	}
	next := "/api/projects/"
	for page := 0; next != "" && page < 20; page++ {
		var list struct {
			Results []struct {
				ID   json.Number `json:"id"`
				Name string      `json:"name"`
			} `json:"results"`
			Next string `json:"next"`
		}
		if err := s.do(ctx, a, http.MethodGet, next, nil, &list); err != nil {
			return nil, err
		}
		for _, p := range list.Results {
			out = append(out, project{ID: p.ID.String(), Name: firstNonEmpty(p.Name, p.ID.String())})
		}
		next = ""
		if u, err := url.Parse(list.Next); err == nil && list.Next != "" {
			next = u.RequestURI()
		}
	}
	return out, nil
}

// errorRow is one issue's counts, by column name.
type errorRow map[string]any

func (r errorRow) str(k string) string {
	switch v := r[k].(type) {
	case string:
		return strings.TrimSpace(v)
	case nil:
		return ""
	default:
		return strings.TrimSpace(fmt.Sprint(v))
	}
}

func (r errorRow) num(k string) float64 {
	switch v := r[k].(type) {
	case float64:
		return v
	case string:
		f, _ := strconv.ParseFloat(v, 64)
		return f
	}
	return 0
}

// exceptionQuery counts each issue's exceptions in the window and before it.
// The issue is error tracking's own id when it has assigned one, its
// fingerprint otherwise, and the type and message as a last resort.
func exceptionQuery(window, baseline time.Duration) string {
	w, b := int64(window.Seconds()), int64(baseline.Seconds())
	return fmt.Sprintf(`SELECT
    coalesce(nullIf(toString(properties.$exception_issue_id), ''), nullIf(toString(properties.$exception_fingerprint), ''),
             concat(ifNull(toString(properties.$exception_type), ''), ': ', ifNull(toString(properties.$exception_message), ''))) AS issue,
    countIf(timestamp >= now() - toIntervalSecond(%[1]d)) AS recent,
    countIf(timestamp < now() - toIntervalSecond(%[1]d)) AS before,
    min(timestamp) AS first_seen,
    argMax(toString(properties.$exception_issue_id), timestamp) AS issue_id,
    argMax(toString(properties.$exception_type), timestamp) AS exc_type,
    argMax(toString(properties.$exception_message), timestamp) AS exc_message,
    argMax(toString(properties.$exception_types), timestamp) AS types,
    argMax(toString(properties.$exception_values), timestamp) AS vals
FROM events
WHERE event = '$exception' AND timestamp >= now() - toIntervalSecond(%[2]d)
GROUP BY issue
HAVING recent > 0
ORDER BY recent DESC
LIMIT 50`, w, b)
}

func (s *Source) checkErrors(ctx context.Context, a Account, p project, res *Result) error {
	rows, err := s.query(ctx, a, p, exceptionQuery(s.cfg.Window, s.cfg.Baseline))
	if err != nil {
		return err
	}
	window := s.cfg.Window
	for _, r := range rows {
		issue := r.str("issue")
		if issue == "" {
			continue
		}
		recent, before := r.num("recent"), r.num("before")
		usual := before * window.Hours() / (s.cfg.Baseline - window).Hours()
		spike := recent >= float64(s.cfg.SpikeMin) && recent >= s.cfg.SpikeFactor*usual
		fresh := before == 0
		if !spike && !fresh {
			continue
		}
		name := exceptionName(r)
		link := ""
		if id := r.str("issue_id"); id != "" {
			link = a.Host + "/project/" + p.ID + "/error_tracking/" + url.PathEscape(id)
		}
		e := sources.Event{
			Source: Name, Account: p.Name, URL: link, At: s.Now(),
			Data: map[string]any{"project_id": p.ID, "project": p.Name, "issue": issue, "issue_id": r.str("issue_id"),
				"recent": recent, "before": before, "usual_per_window": usual, "window": window.String(),
				"first_seen": r.str("first_seen"), "new": fresh},
		}
		seen := s.seenNew
		if spike {
			seen = s.seenSpike
			e.Type, e.Urgent, e.Key = "error.spike", true, "spike:"+p.ID+":"+issue
			e.Title = fmt.Sprintf("PostHog %s: %s is spiking — %d in the last %s (usually ~%.0f)",
				p.Name, name, int(recent), human(window), usual)
			if fresh {
				e.Title = fmt.Sprintf("PostHog %s: new error %s — %d in the last %s", p.Name, name, int(recent), human(window))
			}
		} else {
			e.Type, e.Key = "error.new", "new:"+p.ID+":"+issue
			e.Title = fmt.Sprintf("PostHog %s: new error %s (%d in the last %s)", p.Name, name, int(recent), human(window))
		}
		e.Summary = fmt.Sprintf("%s\nIn the last %s: %d. Over the %s before: %d (about %.1f per %s).",
			e.Title, human(window), int(recent), human(s.cfg.Baseline-window), int(before), usual, human(window))
		if err := s.offer(seen, e, res); err != nil {
			return err
		}
	}
	return nil
}

// exceptionName reads an issue's type and message from whichever properties
// the SDK that sent it sets.
func exceptionName(r errorRow) string {
	typ, msg := r.str("exc_type"), r.str("exc_message")
	if typ == "" {
		typ = firstOf(r.str("types"))
	}
	if msg == "" {
		msg = firstOf(r.str("vals"))
	}
	switch {
	case typ != "" && msg != "":
		return clip(typ+": "+msg, 160)
	case typ != "" || msg != "":
		return clip(typ+msg, 160)
	}
	return "exception " + clip(r.str("issue"), 60)
}

// firstOf reads the first string of a JSON array, or the text itself.
func firstOf(s string) string {
	var list []string
	if json.Unmarshal([]byte(s), &list) == nil {
		for _, v := range list {
			if v = strings.TrimSpace(v); v != "" {
				return v
			}
		}
		return ""
	}
	return s
}

func (s *Source) checkAlerts(ctx context.Context, a Account, p project, res *Result) error {
	var list struct {
		Results []struct {
			ID      string          `json:"id"`
			Name    string          `json:"name"`
			State   string          `json:"state"`
			Enabled *bool           `json:"enabled"`
			Insight json.RawMessage `json:"insight"`
		} `json:"results"`
	}
	if err := s.do(ctx, a, http.MethodGet, "/api/projects/"+url.PathEscape(p.ID)+"/alerts/?limit=100", nil, &list); err != nil {
		var he *httpError
		if errors.As(err, &he) && he.Status == http.StatusNotFound {
			return nil // an instance without alerts
		}
		return err
	}
	for _, al := range list.Results {
		if al.ID == "" || (al.Enabled != nil && !*al.Enabled) {
			continue
		}
		state := strings.ToLower(strings.TrimSpace(al.State))
		key := p.ID + ":" + al.ID
		prev, _, err := s.kv.KVGet(alertStateGroup, key)
		if err != nil {
			return err
		}
		prevState, since, _ := strings.Cut(prev, "|")
		if state == prevState {
			continue
		}
		since = strconv.FormatInt(s.Now().Unix(), 10)
		if err := s.kv.KVSet(alertStateGroup, key, state+"|"+since, 0); err != nil {
			return err
		}
		var typ string
		urgent := false
		switch state {
		case "firing":
			typ, urgent = "alert.firing", true
		case "errored":
			typ = "alert.errored"
		default:
			continue
		}
		var ins struct {
			ID      json.Number `json:"id"`
			ShortID string      `json:"short_id"`
			Name    string      `json:"name"`
			Derived string      `json:"derived_name"`
		}
		_ = json.Unmarshal(al.Insight, &ins) // an id alone leaves this empty
		link := a.Host + "/project/" + p.ID + "/insights"
		if ins.ShortID != "" {
			link += "/" + ins.ShortID + "?alert_id=" + url.QueryEscape(al.ID)
		}
		insight := firstNonEmpty(ins.Name, ins.Derived)
		title := fmt.Sprintf("PostHog %s: alert firing — %s", p.Name, al.Name)
		if typ == "alert.errored" {
			title = fmt.Sprintf("PostHog %s: alert %s could not be checked", p.Name, al.Name)
		}
		if insight != "" {
			title += " (" + insight + ")"
		}
		e := sources.Event{Source: Name, Account: p.Name, Type: typ, Key: "alert:" + key + ":" + since,
			Title: title, Summary: title, URL: link, At: s.Now(), Urgent: urgent,
			Data: map[string]any{"project_id": p.ID, "project": p.Name, "alert_id": al.ID, "alert": al.Name,
				"state": al.State, "insight": insight, "insight_short_id": ins.ShortID}}
		if err := s.offer(s.seenOnce, e, res); err != nil {
			return err
		}
	}
	return nil
}

// anomalyQuery counts yesterday and the seven days before it for the busiest
// events, or for the named ones.
func anomalyQuery(metrics []string, top int) string {
	filter := ""
	if len(metrics) > 0 {
		quoted := make([]string, 0, len(metrics))
		for _, m := range metrics {
			quoted = append(quoted, "'"+strings.NewReplacer(`\`, `\\`, `'`, `\'`).Replace(m)+"'")
		}
		filter = " AND event IN (" + strings.Join(quoted, ", ") + ")"
		top = len(metrics)
	}
	return fmt.Sprintf(`SELECT event,
    countIf(toDate(timestamp) = yesterday()) AS y,
    countIf(toDate(timestamp) < yesterday()) AS prior
FROM events
WHERE timestamp >= toStartOfDay(now()) - toIntervalDay(8) AND timestamp < toStartOfDay(now())%s
GROUP BY event
ORDER BY prior DESC
LIMIT %d`, filter, top)
}

func (s *Source) checkAnomalies(ctx context.Context, a Account, p project, res *Result) error {
	day := s.Now().Format("2006-01-02")
	if last, _, err := s.kv.KVGet(anomalyDayGroup, p.ID); err != nil || last == day {
		return err
	}
	var metrics []string
	for _, m := range s.cfg.Metrics {
		if m = strings.TrimSpace(m); m != "" {
			metrics = append(metrics, m)
		}
	}
	rows, err := s.query(ctx, a, p, anomalyQuery(metrics, s.cfg.AnomalyTop))
	if err != nil {
		return err
	}
	for _, r := range rows {
		event := r.str("event")
		y, usual := r.num("y"), r.num("prior")/7
		if event == "" || usual < float64(s.cfg.AnomalyMin) {
			continue
		}
		change := (y - usual) / usual
		if math.Abs(change) < s.cfg.AnomalyChange {
			continue
		}
		dir := "rose"
		if change < 0 {
			dir = "fell"
		}
		title := fmt.Sprintf("PostHog %s: %s %s %.0f%% yesterday (%d vs ~%.0f a day)", p.Name, event, dir,
			math.Abs(change)*100, int(y), usual)
		e := sources.Event{Source: Name, Account: p.Name, Type: "metric.anomaly", Key: "anomaly:" + p.ID + ":" + event + ":" + day,
			Title: title, Summary: title, URL: a.Host + "/project/" + p.ID + "/events", At: s.Now(),
			Data: map[string]any{"project_id": p.ID, "project": p.Name, "event": event, "yesterday": y,
				"usual": usual, "change": change}}
		if err := s.offer(s.seenOnce, e, res); err != nil {
			return err
		}
	}
	return s.kv.KVSet(anomalyDayGroup, p.ID, day, 0)
}

func (s *Source) offer(seen sources.Seen, e sources.Event, res *Result) error {
	first, err := seen.First(e.Key)
	if err != nil || !first {
		return err
	}
	if err := s.publish(e); err != nil {
		return err
	}
	res.Published++
	return nil
}

// query runs one HogQL query and returns its rows keyed by column.
func (s *Source) query(ctx context.Context, a Account, p project, hogql string) ([]errorRow, error) {
	body := map[string]any{"query": map[string]any{"kind": "HogQLQuery", "query": hogql}, "name": "karmax-source"}
	var out struct {
		Columns []string `json:"columns"`
		Results [][]any  `json:"results"`
	}
	if err := s.do(ctx, a, http.MethodPost, "/api/projects/"+url.PathEscape(p.ID)+"/query/", body, &out); err != nil {
		return nil, err
	}
	rows := make([]errorRow, 0, len(out.Results))
	for _, r := range out.Results {
		row := errorRow{}
		for i, c := range out.Columns {
			if i < len(r) {
				row[c] = r[i]
			}
		}
		rows = append(rows, row)
	}
	return rows, nil
}

type httpError struct {
	Status int
	Path   string
}

func (e *httpError) Error() string {
	switch e.Status {
	case http.StatusUnauthorized:
		return "posthog: the personal API key was refused — create a new one and run `karmax login posthog`"
	case http.StatusForbidden:
		return "posthog: the personal API key may not read " + e.Path + " — it needs the scopes " + Scopes +
			" (and access to the project); run `karmax login posthog` with a key that has them"
	}
	return fmt.Sprintf("posthog: %s answered HTTP %d", e.Path, e.Status)
}

type rateLimited struct{ wait time.Duration }

func (e *rateLimited) Error() string { return fmt.Sprintf("posthog rate limit: retry in %s", e.wait) }

// do makes one API request. The key is never part of an error.
func (s *Source) do(ctx context.Context, a Account, method, path string, body, out any) error {
	return do(ctx, s.HTTP, a, method, path, body, out)
}

func do(ctx context.Context, client *http.Client, a Account, method, path string, body, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	cctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, method, APIHost(a.Host)+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(a.APIKey))
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("posthog: %w", err)
	}
	defer res.Body.Close()
	clean, _, _ := strings.Cut(path, "?")
	switch {
	case res.StatusCode == http.StatusTooManyRequests:
		wait := time.Minute
		if n, err := strconv.Atoi(strings.TrimSpace(res.Header.Get("Retry-After"))); err == nil && n > 0 {
			wait = time.Duration(n) * time.Second
		}
		return &rateLimited{wait: wait}
	case res.StatusCode < 200 || res.StatusCode > 299:
		return &httpError{Status: res.StatusCode, Path: clean}
	}
	if out == nil {
		return nil
	}
	if err := json.NewDecoder(io.LimitReader(res.Body, 16<<20)).Decode(out); err != nil {
		return fmt.Errorf("posthog: %s answered something that is not the expected JSON", clean)
	}
	return nil
}

// Health checks a key the way the source will use it, for `karmax integrations`
// and for `karmax login`, which verifies a key before storing it.
func Health(ctx context.Context, a Account) error {
	if strings.TrimSpace(a.APIKey) == "" {
		return errors.New(OffReason(a.Integration))
	}
	client := &http.Client{Timeout: 30 * time.Second}
	var list struct {
		Results []struct {
			ID json.Number `json:"id"`
		} `json:"results"`
	}
	if err := do(ctx, client, a, http.MethodGet, "/api/projects/", nil, &list); err != nil {
		return err
	}
	for _, id := range a.ProjectIDs {
		if id = strings.TrimSpace(id); id == "" {
			continue
		}
		if err := do(ctx, client, a, http.MethodGet, "/api/projects/"+url.PathEscape(id)+"/", nil, nil); err != nil {
			return fmt.Errorf("project %s: %w", id, err)
		}
	}
	return nil
}

// Status reports each account, for health.
func (s *Source) Status() []Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Status
	for k, st := range s.state {
		integration, _, _ := strings.Cut(k, "|")
		var names []string
		for _, p := range st.projects {
			names = append(names, p.Name)
		}
		out = append(out, Status{Integration: integration, Projects: names, LastOK: st.lastOK,
			LastError: st.lastErr, PausedUntil: st.pausedUntil})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Integration < out[j].Integration })
	return out
}

func human(d time.Duration) string {
	switch {
	case d%(24*time.Hour) == 0:
		if d == 24*time.Hour {
			return "day"
		}
		return fmt.Sprintf("%d days", int(d/(24*time.Hour)))
	case d%time.Hour == 0:
		if d == time.Hour {
			return "hour"
		}
		return fmt.Sprintf("%d hours", int(d/time.Hour))
	}
	return d.String()
}

func clip(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v = strings.TrimSpace(v); v != "" {
			return v
		}
	}
	return ""
}
