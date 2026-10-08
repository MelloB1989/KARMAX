// Package ledger is the fleet's system of record: SQLite at
// ~/.karmax/fleet/fleet.db. fleetd writes it every tick; the orchestrator and
// you read it. Bookkeeping in LLM turns would cost quota and miss things; a
// reconciler loop does neither.
//
// Times are stored as unix seconds.
package ledger

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"

	"github.com/MelloB1989/karmax/fleet/internal/reconcile"
)

// DB is the ledger.
type DB struct{ sql *sql.DB }

const schema = `
CREATE TABLE IF NOT EXISTS state (
  agent TEXT PRIMARY KEY, json TEXT NOT NULL, updated INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS agents (
  name TEXT PRIMARY KEY, host TEXT, account TEXT, models TEXT, container TEXT,
  image TEXT, cc_version TEXT, enabled INTEGER NOT NULL DEFAULT 1, reason TEXT,
  updated INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS sessions (
  id TEXT PRIMARY KEY, agent TEXT NOT NULL, task TEXT, started INTEGER, ended INTEGER,
  reason TEXT, turns INTEGER, input INTEGER, output INTEGER, cache_read INTEGER,
  cache_creation INTEGER, cost_usd REAL, peak_context REAL, cwd TEXT, worktrees TEXT,
  archive_id TEXT);
CREATE INDEX IF NOT EXISTS sessions_agent ON sessions(agent, ended);
CREATE TABLE IF NOT EXISTS quota (
  account TEXT NOT NULL, at INTEGER NOT NULL, five_hour REAL, five_resets INTEGER,
  seven_day REAL, seven_resets INTEGER, source TEXT);
CREATE INDEX IF NOT EXISTS quota_account ON quota(account, at);
CREATE TABLE IF NOT EXISTS usage (
  agent TEXT NOT NULL, model TEXT NOT NULL, hour INTEGER NOT NULL,
  input INTEGER NOT NULL DEFAULT 0, output INTEGER NOT NULL DEFAULT 0,
  cache_read INTEGER NOT NULL DEFAULT 0, cache_creation INTEGER NOT NULL DEFAULT 0,
  cost_usd REAL NOT NULL DEFAULT 0, requests INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (agent, model, hour));
CREATE TABLE IF NOT EXISTS events (
  id INTEGER PRIMARY KEY AUTOINCREMENT, agent TEXT NOT NULL, at INTEGER NOT NULL,
  kind TEXT NOT NULL, session TEXT, detail TEXT);
CREATE INDEX IF NOT EXISTS events_agent ON events(agent, at);
CREATE TABLE IF NOT EXISTS events_daily (
  agent TEXT NOT NULL, day TEXT NOT NULL, kind TEXT NOT NULL, count INTEGER NOT NULL,
  PRIMARY KEY (agent, day, kind));
CREATE TABLE IF NOT EXISTS tasks (
  id TEXT PRIMARY KEY, agent TEXT, assigned INTEGER, closed INTEGER, outcome TEXT,
  branch TEXT, pr TEXT);
CREATE TABLE IF NOT EXISTS relays (
  id INTEGER PRIMARY KEY AUTOINCREMENT, sender TEXT, recipient TEXT, at INTEGER,
  size INTEGER, delivered INTEGER, text TEXT, error TEXT);
CREATE TABLE IF NOT EXISTS host (
  container TEXT NOT NULL, at INTEGER NOT NULL, cpu REAL, mem_bytes INTEGER, mem_pct REAL);
CREATE INDEX IF NOT EXISTS host_at ON host(at);
`

// Open opens (creating) the ledger.
func Open(path string) (*DB, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1) // one writer; fleetd is the only process that writes
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("ledger schema: %w", err)
	}
	return &DB{sql: db}, nil
}

// Close closes the ledger.
func (db *DB) Close() error { return db.sql.Close() }

func unix(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.Unix()
}

func fromUnix(v sql.NullInt64) time.Time {
	if !v.Valid {
		return time.Time{}
	}
	return time.Unix(v.Int64, 0).UTC()
}

// SaveState persists one agent's reconcile state.
func (db *DB) SaveState(st reconcile.State) error {
	b, err := json.Marshal(st)
	if err != nil {
		return err
	}
	_, err = db.sql.Exec(`INSERT INTO state(agent, json, updated) VALUES(?,?,?)
		ON CONFLICT(agent) DO UPDATE SET json=excluded.json, updated=excluded.updated`,
		st.Name, string(b), time.Now().Unix())
	return err
}

// States is every agent's persisted state.
func (db *DB) States() (map[string]reconcile.State, error) {
	rows, err := db.sql.Query(`SELECT agent, json FROM state`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]reconcile.State{}
	for rows.Next() {
		var name, js string
		if err := rows.Scan(&name, &js); err != nil {
			return nil, err
		}
		var st reconcile.State
		if json.Unmarshal([]byte(js), &st) == nil {
			out[name] = st
		}
	}
	return out, rows.Err()
}

// Agent is one row of the agents table.
type Agent struct {
	Name, Host, Account, Container, Image, CCVersion, Reason string
	Models                                                   []string
	Enabled                                                  bool
}

// UpsertAgent records an agent's static facts.
func (db *DB) UpsertAgent(a Agent) error {
	models, _ := json.Marshal(a.Models)
	_, err := db.sql.Exec(`INSERT INTO agents(name, host, account, models, container, image, cc_version, enabled, reason, updated)
		VALUES(?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(name) DO UPDATE SET host=excluded.host, account=excluded.account, models=excluded.models,
		container=excluded.container, image=COALESCE(NULLIF(excluded.image,''), agents.image),
		cc_version=COALESCE(NULLIF(excluded.cc_version,''), agents.cc_version),
		enabled=excluded.enabled, reason=excluded.reason, updated=excluded.updated`,
		a.Name, a.Host, a.Account, string(models), a.Container, a.Image, a.CCVersion, a.Enabled, a.Reason, time.Now().Unix())
	return err
}

// Session is one ended session.
type Session struct {
	ID, Agent, Task, Reason, Cwd, ArchiveID string
	Started, Ended                          time.Time
	Turns                                   int
	Input, Output, CacheRead, CacheCreation int64
	CostUSD, PeakContext                    float64
	Worktrees                               string // JSON
}

// EndSession records a session the fleet ended.
func (db *DB) EndSession(s Session) error {
	_, err := db.sql.Exec(`INSERT INTO sessions(id, agent, task, started, ended, reason, turns, input, output,
		cache_read, cache_creation, cost_usd, peak_context, cwd, worktrees, archive_id)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET ended=excluded.ended, reason=excluded.reason, turns=excluded.turns,
		input=excluded.input, output=excluded.output, cache_read=excluded.cache_read,
		cache_creation=excluded.cache_creation, cost_usd=excluded.cost_usd, worktrees=excluded.worktrees,
		archive_id=excluded.archive_id, task=COALESCE(excluded.task, sessions.task)`,
		s.ID, s.Agent, nullStr(s.Task), unix(s.Started), unix(s.Ended), s.Reason, s.Turns, s.Input, s.Output,
		s.CacheRead, s.CacheCreation, s.CostUSD, s.PeakContext, s.Cwd, s.Worktrees, s.ArchiveID)
	return err
}

func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// History is an agent's ended sessions, newest first.
func (db *DB) History(agent string, limit int) ([]Session, error) {
	rows, err := db.sql.Query(`SELECT id, agent, COALESCE(task,''), started, ended, COALESCE(reason,''), turns,
		input, output, cache_read, cache_creation, cost_usd, COALESCE(cwd,''), COALESCE(archive_id,'')
		FROM sessions WHERE agent=? ORDER BY ended DESC LIMIT ?`, agent, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Session
	for rows.Next() {
		var s Session
		var st, en sql.NullInt64
		if err := rows.Scan(&s.ID, &s.Agent, &s.Task, &st, &en, &s.Reason, &s.Turns, &s.Input, &s.Output,
			&s.CacheRead, &s.CacheCreation, &s.CostUSD, &s.Cwd, &s.ArchiveID); err != nil {
			return nil, err
		}
		s.Started, s.Ended = fromUnix(st), fromUnix(en)
		out = append(out, s)
	}
	return out, rows.Err()
}

// Quota is one sample of an account's windows.
type Quota struct {
	Account                 string
	At                      time.Time
	FiveHour, SevenDay      float64
	FiveResets, SevenResets time.Time
	Source                  string // statusline | stopfailure | harness
}

// AddQuota records a sample.
func (db *DB) AddQuota(q Quota) error {
	_, err := db.sql.Exec(`INSERT INTO quota(account, at, five_hour, five_resets, seven_day, seven_resets, source)
		VALUES(?,?,?,?,?,?,?)`, q.Account, q.At.Unix(), q.FiveHour, unix(q.FiveResets), q.SevenDay, unix(q.SevenResets), q.Source)
	return err
}

// LatestQuota is the newest sample per account.
func (db *DB) LatestQuota() (map[string]Quota, error) {
	rows, err := db.sql.Query(`SELECT q.account, q.at, q.five_hour, q.five_resets, q.seven_day, q.seven_resets, COALESCE(q.source,'')
		FROM quota q JOIN (SELECT account, MAX(at) at FROM quota GROUP BY account) m
		ON q.account=m.account AND q.at=m.at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]Quota{}
	for rows.Next() {
		var q Quota
		var at, fr, sr sql.NullInt64
		if err := rows.Scan(&q.Account, &at, &q.FiveHour, &fr, &q.SevenDay, &sr, &q.Source); err != nil {
			return nil, err
		}
		q.At, q.FiveResets, q.SevenResets = fromUnix(at), fromUnix(fr), fromUnix(sr)
		out[q.Account] = q
	}
	return out, rows.Err()
}

// Usage is tokens and list-price cost, from one API request or a sum.
type Usage struct {
	Agent, Model                            string
	At                                      time.Time
	Input, Output, CacheRead, CacheCreation int64
	CostUSD                                 float64
}

// AddUsage adds to the agent × model × hour bucket.
func (db *DB) AddUsage(u Usage) error {
	hour := u.At.Truncate(time.Hour).Unix()
	_, err := db.sql.Exec(`INSERT INTO usage(agent, model, hour, input, output, cache_read, cache_creation, cost_usd, requests)
		VALUES(?,?,?,?,?,?,?,?,1)
		ON CONFLICT(agent, model, hour) DO UPDATE SET input=input+excluded.input, output=output+excluded.output,
		cache_read=cache_read+excluded.cache_read, cache_creation=cache_creation+excluded.cache_creation,
		cost_usd=cost_usd+excluded.cost_usd, requests=requests+1`,
		u.Agent, u.Model, hour, u.Input, u.Output, u.CacheRead, u.CacheCreation, u.CostUSD)
	return err
}

// UsageRow is a usage sum grouped by agent or model.
type UsageRow struct {
	Key                                     string
	Input, Output, CacheRead, CacheCreation int64
	CostUSD                                 float64
	Requests                                int64
}

// UsageBy sums usage since a time, grouped by "agent" or "model", largest
// cost first.
func (db *DB) UsageBy(by string, since time.Time) ([]UsageRow, error) {
	col := map[string]string{"agent": "agent", "model": "model"}[by]
	if col == "" {
		return nil, fmt.Errorf("usage by %q: want agent or model", by)
	}
	rows, err := db.sql.Query(`SELECT `+col+`, SUM(input), SUM(output), SUM(cache_read), SUM(cache_creation),
		SUM(cost_usd), SUM(requests) FROM usage WHERE hour >= ? GROUP BY `+col+` ORDER BY SUM(cost_usd) DESC, `+col,
		since.Truncate(time.Hour).Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []UsageRow
	for rows.Next() {
		var r UsageRow
		if err := rows.Scan(&r.Key, &r.Input, &r.Output, &r.CacheRead, &r.CacheCreation, &r.CostUSD, &r.Requests); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// Event is one hook event, OTel error or fleetd action.
type Event struct {
	Agent   string    `json:"agent"`
	At      time.Time `json:"at"`
	Kind    string    `json:"kind"`
	Session string    `json:"session,omitempty"`
	Detail  string    `json:"detail,omitempty"` // JSON
}

// AddEvent records an event.
func (db *DB) AddEvent(e Event) error {
	_, err := db.sql.Exec(`INSERT INTO events(agent, at, kind, session, detail) VALUES(?,?,?,?,?)`,
		e.Agent, e.At.Unix(), e.Kind, nullStr(e.Session), nullStr(e.Detail))
	return err
}

// Events is an agent's events since a time, oldest first. An empty agent
// means every agent.
func (db *DB) Events(agent string, since time.Time) ([]Event, error) {
	rows, err := db.sql.Query(`SELECT agent, at, kind, COALESCE(session,''), COALESCE(detail,'') FROM events
		WHERE (?='' OR agent=?) AND at >= ? ORDER BY at, id`, agent, agent, since.Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		var e Event
		var at int64
		if err := rows.Scan(&e.Agent, &at, &e.Kind, &e.Session, &e.Detail); err != nil {
			return nil, err
		}
		e.At = time.Unix(at, 0).UTC()
		out = append(out, e)
	}
	return out, rows.Err()
}

// DailyCount is events of one kind on one day, kept after the raw rows age out.
type DailyCount struct {
	Day, Kind string
	Count     int
}

// DailyEventCounts is an agent's aged-out event counts.
func (db *DB) DailyEventCounts(agent string) ([]DailyCount, error) {
	rows, err := db.sql.Query(`SELECT day, kind, count FROM events_daily WHERE agent=? ORDER BY day, kind`, agent)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DailyCount
	for rows.Next() {
		var d DailyCount
		if err := rows.Scan(&d.Day, &d.Kind, &d.Count); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// Task is one KARMAX task handed to an agent.
type Task struct {
	ID, Agent, Outcome, Branch, PR string
	Assigned, Closed               time.Time
}

// OpenTask records an assignment.
func (db *DB) OpenTask(id, agent string, at time.Time) error {
	_, err := db.sql.Exec(`INSERT INTO tasks(id, agent, assigned) VALUES(?,?,?)
		ON CONFLICT(id) DO UPDATE SET agent=excluded.agent, assigned=excluded.assigned, closed=NULL, outcome=NULL`,
		id, agent, at.Unix())
	return err
}

// CloseTask records a task's end.
func (db *DB) CloseTask(id string, at time.Time, outcome, branch, pr string) error {
	_, err := db.sql.Exec(`INSERT INTO tasks(id, closed, outcome, branch, pr) VALUES(?,?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET closed=excluded.closed, outcome=excluded.outcome,
		branch=COALESCE(excluded.branch, tasks.branch), pr=COALESCE(excluded.pr, tasks.pr)`,
		id, at.Unix(), outcome, nullStr(branch), nullStr(pr))
	return err
}

// Task reads one task.
func (db *DB) Task(id string) (*Task, error) {
	var t Task
	var as, cl sql.NullInt64
	err := db.sql.QueryRow(`SELECT id, COALESCE(agent,''), assigned, closed, COALESCE(outcome,''), COALESCE(branch,''), COALESCE(pr,'')
		FROM tasks WHERE id=?`, id).Scan(&t.ID, &t.Agent, &as, &cl, &t.Outcome, &t.Branch, &t.PR)
	if err != nil {
		return nil, err
	}
	t.Assigned, t.Closed = fromUnix(as), fromUnix(cl)
	return &t, nil
}

// Relay is one cross-host message.
type Relay struct {
	From, To, Text, Error string
	At                    time.Time
	Delivered             bool
}

// AddRelay records a relayed message.
func (db *DB) AddRelay(r Relay) error {
	_, err := db.sql.Exec(`INSERT INTO relays(sender, recipient, at, size, delivered, text, error) VALUES(?,?,?,?,?,?,?)`,
		r.From, r.To, r.At.Unix(), len(r.Text), r.Delivered, r.Text, nullStr(r.Error))
	return err
}

// HostSample is one container's resource use.
type HostSample struct {
	Container string
	At        time.Time
	CPU       float64 // percent
	MemBytes  int64
	MemPct    float64
}

// AddHostSample records a sample.
func (db *DB) AddHostSample(h HostSample) error {
	_, err := db.sql.Exec(`INSERT INTO host(container, at, cpu, mem_bytes, mem_pct) VALUES(?,?,?,?,?)`,
		h.Container, h.At.Unix(), h.CPU, h.MemBytes, h.MemPct)
	return err
}

// Retention is how long raw rows are kept.
type Retention struct {
	EventsRaw, QuotaRaw, Relays, HostSamples time.Duration
}

// Retain ages the ledger: raw events become daily counts, old quota samples
// thin to one per hour, relays and host samples expire. Sessions, tasks,
// usage and agents are kept forever.
func (db *DB) Retain(now time.Time, r Retention) error {
	tx, err := db.sql.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	cut := now.Add(-r.EventsRaw).Unix()
	if _, err := tx.Exec(`INSERT INTO events_daily(agent, day, kind, count)
		SELECT agent, date(at, 'unixepoch'), kind, COUNT(*) FROM events WHERE at < ? GROUP BY 1, 2, 3
		ON CONFLICT(agent, day, kind) DO UPDATE SET count=count+excluded.count`, cut); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM events WHERE at < ?`, cut); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM quota WHERE at < ? AND rowid NOT IN (
		SELECT MIN(rowid) FROM quota WHERE at < ? GROUP BY account, at / 3600)`,
		now.Add(-r.QuotaRaw).Unix(), now.Add(-r.QuotaRaw).Unix()); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM relays WHERE at < ?`, now.Add(-r.Relays).Unix()); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM host WHERE at < ?`, now.Add(-r.HostSamples).Unix()); err != nil {
		return err
	}
	return tx.Commit()
}
