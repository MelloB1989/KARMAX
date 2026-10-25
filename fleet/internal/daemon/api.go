package daemon

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/MelloB1989/karmax/fleet/internal/archive"
	"github.com/MelloB1989/karmax/fleet/internal/hostctl"
	"github.com/MelloB1989/karmax/fleet/internal/ledger"
	"github.com/MelloB1989/karmax/fleet/internal/recreate"
)

// The reads and actions behind fleetd's HTTP routes. Each is one method, so
// the route and any other caller do the same thing; the defaults live here.

const (
	defaultEvents = 200
	maxEvents     = 1000
)

// EventsQuery selects events for the stream. Kinds are exact, or prefixes
// when they end in ".". With After set the result is the next page of the
// ledger after that id; with Newest it is the last Limit matches. Either
// way it is oldest first.
type EventsQuery struct {
	Agent  string
	Since  time.Time
	After  int64
	Kinds  []string
	Limit  int // 0: 200 for a cursor or Newest, 1000 for a window; at most 1000
	Newest bool
}

func (f *Fleet) Events(q EventsQuery) ([]ledger.Event, error) {
	limit := q.Limit
	if limit <= 0 {
		limit = maxEvents
		if q.After > 0 || q.Newest {
			limit = defaultEvents
		}
	}
	limit = min(limit, maxEvents)
	evs, err := f.db.EventsWhere(ledger.EventQuery{Agent: q.Agent, Since: q.Since, After: q.After,
		Kinds: q.Kinds, Limit: limit, Newest: q.Newest})
	if evs == nil {
		evs = []ledger.Event{}
	}
	return evs, err
}

// Usage sums usage since a time, by "agent" (default) or "model".
func (f *Fleet) Usage(by string, since time.Time) ([]ledger.UsageRow, error) {
	if by == "" {
		by = "agent"
	}
	return f.db.UsageBy(by, since)
}

// History is an agent's ended sessions, newest first (n defaults to 20).
func (f *Fleet) History(agent string, n int) ([]ledger.Session, error) {
	if n <= 0 {
		n = 20
	}
	return f.db.History(agent, n)
}

// Quota is each account's latest windows.
// Quota is each account's latest windows, aged at their reset.
func (f *Fleet) Quota() (map[string]ledger.Quota, error) {
	qs, err := f.db.LatestQuota()
	now := time.Now()
	for k, q := range qs {
		qs[k] = q.Aged(now)
	}
	return qs, err
}

// Tasks lists tasks, newest first, of one assigner when given. n defaults
// to everything when open, else 50.
func (f *Fleet) Tasks(open bool, agent, assigner string, n int) ([]ledger.Task, error) {
	if n <= 0 && !open {
		n = 50
	}
	return f.db.TasksBy(open, agent, assigner, n)
}

// ActivityReply is event counts per agent in fixed buckets.
type ActivityReply struct {
	BucketS int                        `json:"bucket_s"`
	Agents  map[string][]ledger.Bucket `json:"agents"`
}

// Activity counts events per agent since a time (zero: the last 24 h) in
// buckets (zero: 15 m).
func (f *Fleet) Activity(since time.Time, bucket time.Duration, now time.Time) (ActivityReply, error) {
	if bucket <= 0 {
		bucket = 15 * time.Minute
	}
	if since.IsZero() {
		since = now.Add(-24 * time.Hour)
	}
	a, err := f.db.Activity(since, bucket)
	if a == nil {
		a = map[string][]ledger.Bucket{}
	}
	return ActivityReply{BucketS: int(bucket.Seconds()), Agents: a}, err
}

// TodayReply is the fleet's totals since local midnight.
type TodayReply struct {
	Since        time.Time `json:"since"`
	TasksClosed  int       `json:"tasks_closed"`
	SpendUSD     float64   `json:"spend_usd"`
	OutputTokens int64     `json:"output_tokens"`
}

// Today counts tasks closed and the cost and output of sessions ended since
// local midnight (in now's location).
func (f *Fleet) Today(now time.Time) (TodayReply, error) {
	y, m, d := now.Date()
	since := time.Date(y, m, d, 0, 0, 0, 0, now.Location())
	t, err := f.db.Totals(since)
	return TodayReply{Since: since.UTC(), TasksClosed: t.TasksClosed, SpendUSD: t.SpendUSD, OutputTokens: t.OutputTokens}, err
}

// Peek is the last n lines (default 80, at most 2000) of an agent's pane.
func (f *Fleet) Peek(ctx context.Context, name string, n int) (string, error) {
	if err := f.known(name); err != nil {
		return "", err
	}
	if n <= 0 {
		n = 80
	}
	return f.box(name).Peek(ctx, min(n, 2000))
}

// Prompt types text into an agent's session as the operator.
func (f *Fleet) Prompt(ctx context.Context, name, text string) error {
	if err := f.known(name); err != nil {
		return err
	}
	if strings.TrimSpace(text) == "" {
		return fmt.Errorf("prompt needs text")
	}
	return f.box(name).Prompt(ctx, text)
}

// RotatePlan says what a rotation does: it closes the agent's open task as
// rotated, at the next idle tick.
type RotatePlan struct {
	Closes string `json:"closes"`
	When   string `json:"when"`
}

// Rotate rotates an agent, or with dryRun only says what it would do.
func (f *Fleet) Rotate(name string, dryRun bool, now time.Time) (RotatePlan, error) {
	if err := f.known(name); err != nil {
		return RotatePlan{}, err
	}
	plan := RotatePlan{Closes: f.State(name).Task, When: "next idle tick"}
	if dryRun {
		return plan, nil
	}
	return plan, f.Done(name, "", "rotated", "", "", now)
}

// ArchiveDetail is an archive's manifest and the size of its transcript.
type ArchiveDetail struct {
	*archive.Manifest
	TranscriptLines int `json:"transcript_lines"`
}

// Archives lists archives since a time (zero: all), newest first.
func (f *Fleet) Archives(since time.Time) ([]*archive.Manifest, error) {
	all, err := archive.List(f.archiveRoot(), since)
	if all == nil {
		all = []*archive.Manifest{}
	}
	return all, err
}

// Archive reads one archive: manifest and transcript length, verified.
func (f *Fleet) Archive(id string) (*ArchiveDetail, error) {
	m, err := archive.Load(f.archiveRoot(), id)
	if err != nil {
		return nil, fmt.Errorf("archive %q: %w", id, err)
	}
	tr, err := archive.ReadTranscript(f.archiveRoot(), id)
	if err != nil {
		return nil, err
	}
	return &ArchiveDetail{Manifest: m, TranscriptLines: bytes.Count(tr.Main, []byte("\n"))}, nil
}

// PruneArchives removes archives made before a time and counts them.
func (f *Fleet) PruneArchives(before time.Time) (int, error) {
	return archive.Prune(f.archiveRoot(), before)
}

// HostRun runs commands for Up and Down; tests replace it.
var HostRun hostctl.Runner = hostctl.ExecRunner

// HostDocker runs the plan's read-only docker calls; tests replace it.
var HostDocker recreate.Runner = hostctl.RealDocker

// Render writes the compose files and agent env files, and says what it wrote.
func (f *Fleet) Render() (string, error) {
	var out bytes.Buffer
	err := hostctl.Render(f.cfg, "", &out)
	return out.String(), err
}

// Up brings hosts (none: all) up; Down takes them down. Both return the
// commands run and their output.
func (f *Fleet) Up(ctx context.Context, hosts []string) (string, error) {
	return f.compose(ctx, true, hosts)
}
func (f *Fleet) Down(ctx context.Context, hosts []string) (string, error) {
	return f.compose(ctx, false, hosts)
}

func (f *Fleet) compose(ctx context.Context, up bool, hosts []string) (string, error) {
	var out bytes.Buffer
	err := hostctl.Update(ctx, f.cfg, up, hosts, hostctl.Options{Run: HostRun, Docker: HostDocker}, &out)
	return out.String(), err
}
