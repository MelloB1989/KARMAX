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
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
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
  branch TEXT, pr TEXT, assigner TEXT);
CREATE TABLE IF NOT EXISTS model_override (
  agent TEXT PRIMARY KEY, model TEXT NOT NULL, updated INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS relays (
  id INTEGER PRIMARY KEY AUTOINCREMENT, sender TEXT, recipient TEXT, at INTEGER,
  size INTEGER, delivered INTEGER, text TEXT, error TEXT);
CREATE TABLE IF NOT EXISTS cursors (
  task TEXT NOT NULL, reader TEXT NOT NULL, seen INTEGER NOT NULL, PRIMARY KEY (task, reader));
CREATE TABLE IF NOT EXISTS host (
  container TEXT NOT NULL, at INTEGER NOT NULL, cpu REAL, mem_bytes INTEGER, mem_pct REAL);
CREATE INDEX IF NOT EXISTS host_at ON host(at);
CREATE TABLE IF NOT EXISTS approvals (
  id INTEGER PRIMARY KEY AUTOINCREMENT, agent TEXT NOT NULL, task TEXT, tool TEXT NOT NULL,
  args TEXT, reason TEXT, state TEXT NOT NULL, decided_by TEXT, note TEXT,
  at INTEGER NOT NULL, decided INTEGER, expires INTEGER NOT NULL);
CREATE INDEX IF NOT EXISTS approvals_state ON approvals(state, id);
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
	if _, err := db.Exec(schema + secretsSchema + accessSchema); err != nil {
		db.Close()
		return nil, fmt.Errorf("ledger schema: %w", err)
	}
	if err := migrate(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("ledger migration: %w", err)
	}
	return &DB{sql: db}, nil
}

// migrate adds the columns a ledger made by an older fleetd lacks.
func migrate(db *sql.DB) error {
	adds := []struct{ table, column, decl string }{
		{"tasks", "assigner", "TEXT"},
		{"tasks", "repo", "TEXT"},
		{"tasks", "model", "TEXT"},
		{"relays", "task", "TEXT"},
		{"relays", "kind", "TEXT"},
		{"relays", "escalated", "INTEGER"},
		{"relays", "alerted", "INTEGER"},
		{"relays", "delivered_at", "INTEGER"},
		{"tasks", "admin_work", "INTEGER"},
	}
	for _, a := range adds {
		var n int
		if err := db.QueryRow(`SELECT count(*) FROM pragma_table_info(?) WHERE name=?`, a.table, a.column).Scan(&n); err != nil {
			return err
		}
		if n == 0 {
			if _, err := db.Exec(`ALTER TABLE ` + a.table + ` ADD COLUMN ` + a.column + ` ` + a.decl); err != nil {
				return err
			}
		}
	}
	return nil
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
	ID            string    `json:"id"`
	Agent         string    `json:"agent"`
	Task          string    `json:"task,omitempty"`
	Reason        string    `json:"reason"`
	Cwd           string    `json:"cwd,omitempty"`
	ArchiveID     string    `json:"archive_id,omitempty"`
	Started       time.Time `json:"started,omitzero"`
	Ended         time.Time `json:"ended,omitzero"`
	Turns         int       `json:"turns"`
	Input         int64     `json:"input"`
	Output        int64     `json:"output"`
	CacheRead     int64     `json:"cache_read"`
	CacheCreation int64     `json:"cache_creation"`
	CostUSD       float64   `json:"cost_usd"`
	PeakContext   float64   `json:"peak_context,omitempty"`
	Worktrees     string    `json:"worktrees,omitempty"` // JSON
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

// Aged is q as of now: a window whose reset has passed is a new one that no
// turn has reported yet, so it is shown reset, not with the use it had.
func (q Quota) Aged(now time.Time) Quota {
	if !q.FiveResets.IsZero() && !now.Before(q.FiveResets) {
		q.FiveHour, q.FiveResets = 0, time.Time{}
	}
	if !q.SevenResets.IsZero() && !now.Before(q.SevenResets) {
		q.SevenDay, q.SevenResets = 0, time.Time{}
	}
	return q
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

// Event is one hook event, OTel error or fleetd action. ID is its place in
// the ledger (the cursor for EventsAfter); it is 0 until recorded.
type Event struct {
	ID      int64     `json:"id"`
	Agent   string    `json:"agent"`
	At      time.Time `json:"at"`
	Kind    string    `json:"kind"`
	Session string    `json:"session,omitempty"`
	Detail  string    `json:"detail,omitempty"` // JSON
}

// MarshalJSON emits Detail as JSON when it is JSON (it nearly always is),
// and as a string otherwise.
func (e Event) MarshalJSON() ([]byte, error) {
	type plain Event
	out := struct {
		plain
		Detail json.RawMessage `json:"detail,omitempty"`
	}{plain: plain(e)}
	switch {
	case e.Detail == "":
	case json.Valid([]byte(e.Detail)):
		out.Detail = json.RawMessage(e.Detail)
	default:
		out.Detail, _ = json.Marshal(e.Detail)
	}
	return json.Marshal(out)
}

// UnmarshalJSON reads what MarshalJSON writes.
func (e *Event) UnmarshalJSON(b []byte) error {
	type plain Event
	in := struct {
		*plain
		Detail json.RawMessage `json:"detail"`
	}{plain: (*plain)(e)}
	if err := json.Unmarshal(b, &in); err != nil {
		return err
	}
	e.Detail = string(in.Detail)
	if len(in.Detail) > 0 && in.Detail[0] == '"' {
		var s string
		if json.Unmarshal(in.Detail, &s) == nil {
			e.Detail = s
		}
	}
	return nil
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
	return db.EventsWhere(EventQuery{Agent: agent, Since: since})
}

// EventQuery selects events. Every field narrows it; zero values match all.
type EventQuery struct {
	Agent string
	Since time.Time
	After int64    // only ids above this
	Kinds []string // exact kinds, or prefixes when they end in "."
	Limit int      // 0: no limit
	// Newest takes the last Limit matches instead of the first; either way
	// the result is oldest first.
	Newest bool
}

// EventsWhere is the events a query selects, ascending by id.
func (db *DB) EventsWhere(q EventQuery) ([]Event, error) {
	where := []string{"(?='' OR agent=?)", "at >= ?", "id > ?"}
	args := []any{q.Agent, q.Agent, q.Since.Unix(), q.After}
	if len(q.Kinds) > 0 {
		var ors []string
		for _, k := range q.Kinds {
			if strings.HasSuffix(k, ".") {
				ors = append(ors, `kind LIKE ? ESCAPE '\'`)
				args = append(args, likeEscape(k)+"%")
			} else {
				ors = append(ors, "kind = ?")
				args = append(args, k)
			}
		}
		where = append(where, "("+strings.Join(ors, " OR ")+")")
	}
	order := "id"
	if q.Newest {
		order = "id DESC"
	}
	query := `SELECT id, agent, at, kind, COALESCE(session,''), COALESCE(detail,'') FROM events WHERE ` +
		strings.Join(where, " AND ") + ` ORDER BY ` + order
	if q.Limit > 0 {
		query += fmt.Sprintf(" LIMIT %d", q.Limit)
	}
	rows, err := db.sql.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		var e Event
		var at int64
		if err := rows.Scan(&e.ID, &e.Agent, &at, &e.Kind, &e.Session, &e.Detail); err != nil {
			return nil, err
		}
		e.At = time.Unix(at, 0).UTC()
		out = append(out, e)
	}
	if q.Newest {
		slices.Reverse(out)
	}
	return out, rows.Err()
}

func likeEscape(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}

// Bucket is the events one agent had in one time bucket.
type Bucket struct {
	T time.Time `json:"t"`
	N int       `json:"n"`
}

// Activity counts events since a time, per agent, in buckets of the given
// size (aligned to the Unix epoch), oldest first.
func (db *DB) Activity(since time.Time, bucket time.Duration) (map[string][]Bucket, error) {
	b := int64(bucket.Seconds())
	if b <= 0 {
		return nil, fmt.Errorf("bucket must be at least a second")
	}
	rows, err := db.sql.Query(`SELECT agent, (at/?)*?, COUNT(*) FROM events WHERE at >= ?
		GROUP BY 1, 2 ORDER BY 1, 2`, b, b, since.Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]Bucket{}
	for rows.Next() {
		var agent string
		var t int64
		var n int
		if err := rows.Scan(&agent, &t, &n); err != nil {
			return nil, err
		}
		out[agent] = append(out[agent], Bucket{T: time.Unix(t, 0).UTC(), N: n})
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

// Task is one task handed to an agent. Assigner is who gave it: the
// operator, or the orchestrator by name; the agent reports there.
type Task struct {
	ID       string    `json:"id"`
	Agent    string    `json:"agent"`
	Assigner string    `json:"assigner"`
	Outcome  string    `json:"outcome,omitempty"`
	Repo     string    `json:"repo,omitempty"`
	Model    string    `json:"model,omitempty"` // the model the task asked for (run --model)
	Branch   string    `json:"branch,omitempty"`
	PR       string    `json:"pr,omitempty"`
	Assigned time.Time `json:"assigned,omitzero"`
	Closed   time.Time `json:"closed,omitzero"`
	// AdminWork is a task assigned with the full scope (the full token or an
	// admin device) to an ordinary agent: its agent may use the operator's
	// memory, gateway and secrets. Never a guest's, never a sandbox's.
	AdminWork bool `json:"admin_work"`
}

// OpenTask records an assignment.
func (db *DB) OpenTask(id, agent, assigner string, at time.Time) error {
	_, err := db.sql.Exec(`INSERT INTO tasks(id, agent, assigned, assigner) VALUES(?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET agent=excluded.agent, assigned=excluded.assigned,
		assigner=excluded.assigner, closed=NULL, outcome=NULL, admin_work=NULL`,
		id, agent, at.Unix(), nullStr(assigner))
	return err
}

// SetTaskAdminWork records whether a task is admin work (Task.AdminWork).
func (db *DB) SetTaskAdminWork(id string, admin bool) error {
	_, err := db.sql.Exec(`UPDATE tasks SET admin_work=? WHERE id=?`, admin, id)
	return err
}

// SetTaskRepo records which repo a task works in.
func (db *DB) SetTaskRepo(id, repo string) error {
	_, err := db.sql.Exec(`UPDATE tasks SET repo=? WHERE id=?`, nullStr(repo), id)
	return err
}

// SetTaskModel records the model a task asked for.
func (db *DB) SetTaskModel(id, model string) error {
	_, err := db.sql.Exec(`UPDATE tasks SET model=? WHERE id=?`, nullStr(model), id)
	return err
}

// ModelOverride is the model set for an agent with fleetctl model, "" if none.
func (db *DB) ModelOverride(agent string) (string, error) {
	var m string
	err := db.sql.QueryRow(`SELECT model FROM model_override WHERE agent=?`, agent).Scan(&m)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return m, err
}

// SetModelOverride keeps an agent's model override across restarts; an
// empty model clears it.
func (db *DB) SetModelOverride(agent, model string, at time.Time) error {
	if model == "" {
		_, err := db.sql.Exec(`DELETE FROM model_override WHERE agent=?`, agent)
		return err
	}
	_, err := db.sql.Exec(`INSERT INTO model_override(agent, model, updated) VALUES(?,?,?)
		ON CONFLICT(agent) DO UPDATE SET model=excluded.model, updated=excluded.updated`, agent, model, at.Unix())
	return err
}

// LastRepo is the repo of the agent's most recent task, "" if it has none.
func (db *DB) LastRepo(agent string) (string, error) {
	var repo string
	err := db.sql.QueryRow(`SELECT COALESCE(repo,'') FROM tasks WHERE agent=? ORDER BY assigned DESC, id DESC LIMIT 1`, agent).Scan(&repo)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return repo, err
}

// CloseTask records a task's end.
func (db *DB) CloseTask(id string, at time.Time, outcome, branch, pr string) error {
	_, err := db.sql.Exec(`INSERT INTO tasks(id, closed, outcome, branch, pr) VALUES(?,?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET closed=excluded.closed, outcome=excluded.outcome,
		branch=COALESCE(excluded.branch, tasks.branch), pr=COALESCE(excluded.pr, tasks.pr)`,
		id, at.Unix(), outcome, nullStr(branch), nullStr(pr))
	if err != nil {
		return err
	}
	return db.endTaskGrants(id)
}

// SupersedeOpen closes every task of agent that is still open, except one,
// as "superseded" — the agent was given new work — and returns their ids.
func (db *DB) SupersedeOpen(agent, except string, at time.Time) ([]string, error) {
	rows, err := db.sql.Query(`SELECT id FROM tasks WHERE agent=? AND closed IS NULL AND id<>? ORDER BY assigned, id`, agent, except)
	if err != nil {
		return nil, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	for _, id := range ids {
		if _, err := db.sql.Exec(`UPDATE tasks SET closed=?, outcome='superseded' WHERE id=? AND closed IS NULL`, at.Unix(), id); err != nil {
			return ids, err
		}
	}
	return ids, db.endTaskGrants(ids...)
}

// Task reads one task.
func (db *DB) Task(id string) (*Task, error) {
	var t Task
	var as, cl sql.NullInt64
	err := db.sql.QueryRow(`SELECT id, COALESCE(agent,''), COALESCE(assigner,''), assigned, closed, COALESCE(outcome,''),
		COALESCE(branch,''), COALESCE(pr,''), COALESCE(repo,''), COALESCE(model,''), COALESCE(admin_work,0) FROM tasks WHERE id=?`, id).Scan(&t.ID, &t.Agent, &t.Assigner, &as, &cl,
		&t.Outcome, &t.Branch, &t.PR, &t.Repo, &t.Model, &t.AdminWork)
	if err != nil {
		return nil, err
	}
	t.Assigned, t.Closed = fromUnix(as), fromUnix(cl)
	return &t, nil
}

// Tasks lists tasks, newest assignment first. open keeps only those not yet
// closed; an agent of "" means every agent; limit <= 0 means all.
func (db *DB) Tasks(open bool, agent string, limit int) ([]Task, error) {
	return db.TasksBy(open, agent, "", limit)
}

// TasksBy is Tasks of one assigner (any, when empty): the tasks a person
// on the tailnet asked for, from any of their sessions (OwnerOf).
func (db *DB) TasksBy(open bool, agent, assigner string, limit int) ([]Task, error) {
	q := `SELECT id, COALESCE(agent,''), COALESCE(assigner,''), assigned, closed, COALESCE(outcome,''),
		COALESCE(branch,''), COALESCE(pr,''), COALESCE(repo,''), COALESCE(model,''), COALESCE(admin_work,0) FROM tasks WHERE agent IS NOT NULL AND (?='' OR agent=?)
		AND (?='' OR assigner=? OR assigner LIKE ? ESCAPE '\')`
	if open {
		q += ` AND closed IS NULL`
	}
	q += ` ORDER BY assigned DESC, id DESC`
	args := []any{agent, agent, assigner, assigner, likeEscape(assigner) + "/%"}
	if limit > 0 {
		q += ` LIMIT ?`
		args = append(args, limit)
	}
	rows, err := db.sql.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Task{}
	for rows.Next() {
		var t Task
		var as, cl sql.NullInt64
		if err := rows.Scan(&t.ID, &t.Agent, &t.Assigner, &as, &cl, &t.Outcome, &t.Branch, &t.PR, &t.Repo, &t.Model, &t.AdminWork); err != nil {
			return nil, err
		}
		t.Assigned, t.Closed = fromUnix(as), fromUnix(cl)
		out = append(out, t)
	}
	return out, rows.Err()
}

// DayTotals is what the fleet did since a time.
type DayTotals struct {
	TasksClosed  int
	SpendUSD     float64
	OutputTokens int64
}

// Totals counts tasks closed, and the cost and output of sessions ended,
// since a time.
func (db *DB) Totals(since time.Time) (DayTotals, error) {
	var d DayTotals
	if err := db.sql.QueryRow(`SELECT COUNT(*) FROM tasks WHERE closed >= ?`, since.Unix()).Scan(&d.TasksClosed); err != nil {
		return d, err
	}
	err := db.sql.QueryRow(`SELECT COALESCE(SUM(cost_usd),0), COALESCE(SUM(output),0) FROM sessions WHERE ended >= ?`,
		since.Unix()).Scan(&d.SpendUSD, &d.OutputTokens)
	return d, err
}

// Relay is one message between sessions. One to "operator" is stored for
// the operator's inbox instead of being delivered anywhere.
type Relay struct {
	ID        int64     `json:"id"`
	From      string    `json:"from"`
	To        string    `json:"to"`
	Text      string    `json:"text"`
	Error     string    `json:"error,omitempty"`
	At        time.Time `json:"at"`
	Delivered bool      `json:"delivered"`
	// Task is the task its sender had when it wrote: a report belongs to it.
	Task string `json:"task,omitempty"`
	// Kind is what the message is: KindReport unless its sender said otherwise.
	Kind string `json:"kind,omitempty"`
	// Held: fleetd has not delivered it yet and is keeping it for a receiver
	// that cannot take it (Error says held or interrupted).
	Held bool `json:"held,omitempty"`
	// Alerted: you were told it has been held a long time (HeldBefore).
	Alerted bool `json:"-"`
}

// What a message filed under a task is. An agent's report is the default; a
// question waits for an answer; progress is a note on the way. An answer is
// what the assigner sent to a question the agent is blocked on (fleetctl ask).
const (
	KindReport   = "report"
	KindQuestion = "question"
	KindProgress = "progress"
	KindAnswer   = "answer"
)

// ValidKind reports whether an agent may send a message of that kind
// (fleetctl tell --kind); "" is a report.
func ValidKind(k string) bool {
	return k == "" || k == KindReport || k == KindQuestion || k == KindProgress
}

// Operator is the inbox name: messages to it wait for the operator.
const Operator = "operator"

// IsPerson reports whether a name is a person rather than a session: the
// operator, or someone on the tailnet by their login (you@example.com; no
// agent or orchestrator name has an @). A message to a person is filed on
// the ledger, where they read it (wait, result); nothing is relayed.
func IsPerson(name string) bool { return name == Operator || strings.Contains(name, "@") }

// OwnerOf is the person behind an assigner name. A person's own session is
// named login/session (ram@example.com/review): the session is theirs, and so
// is everything it asked for. Any other name is its own owner.
func OwnerOf(assigner string) string {
	owner, _, _ := strings.Cut(assigner, "/")
	return owner
}

// OwnedBy reports whether a task's assigner is login or one of their sessions.
func OwnedBy(assigner, login string) bool { return login != "" && OwnerOf(assigner) == login }

// IsSession reports whether a name is a session that assigned a task: a
// Claude session that ran fleetctl run under a name of its own. Like a
// person's, its inbox is the ledger.
func (db *DB) IsSession(name string) bool {
	if name == "" || IsPerson(name) {
		return false
	}
	var one int
	return db.sql.QueryRow(`SELECT 1 FROM tasks WHERE assigner=? LIMIT 1`, name).Scan(&one) == nil
}

// Inbox is the messages to the operator with an id above after, oldest first.
func (db *DB) Inbox(after int64) ([]Relay, error) { return db.InboxOf(Operator, after) }

// InboxOf is Inbox of any inbox: a person's, or a session's.
func (db *DB) InboxOf(to string, after int64) ([]Relay, error) {
	rows, err := db.sql.Query(`SELECT id, COALESCE(sender,''), at, COALESCE(text,''), COALESCE(task,''), COALESCE(kind,'report') FROM relays
		WHERE recipient=? AND id > ? ORDER BY id`, to, after)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Relay
	for rows.Next() {
		r := Relay{To: to, Delivered: true}
		var at int64
		if err := rows.Scan(&r.ID, &r.From, &at, &r.Text, &r.Task, &r.Kind); err != nil {
			return nil, err
		}
		r.At = time.Unix(at, 0)
		out = append(out, r)
	}
	return out, rows.Err()
}

// TaskReports is what a task's agent wrote under it, to whomever, with an id
// above after, oldest first.
func (db *DB) TaskReports(task string, after int64) ([]Relay, error) {
	return db.taskRelays(`AND sender = (SELECT agent FROM tasks WHERE id=relays.task)`, task, after)
}

// TaskThread is the whole conversation filed under a task, both directions,
// oldest first.
func (db *DB) TaskThread(task string) ([]Relay, error) {
	return db.taskRelays(``, task, 0)
}

// LastInstruction is the id of the newest message filed under a task that its
// agent did not write (the brief, a follow-up, a cancel), 0 if none.
func (db *DB) LastInstruction(task string) (int64, error) {
	var id int64
	err := db.sql.QueryRow(`SELECT COALESCE(MAX(id),0) FROM relays WHERE task=?
		AND sender IS NOT (SELECT agent FROM tasks WHERE id=?)`, task, task).Scan(&id)
	return id, err
}

// Owes reports whether a task's agent owes it the next word: the newest
// message filed under the task is one the agent was given (the brief, a
// follow-up), not one it wrote. An agent that owes is still working on it.
func (db *DB) Owes(task string) (bool, error) {
	var owes bool
	err := db.sql.QueryRow(`SELECT COALESCE(MAX(CASE WHEN r.sender IS NOT t.agent AND r.delivered THEN r.id END), 0)
		> COALESCE(MAX(CASE WHEN r.sender = t.agent THEN r.id END), 0)
		FROM relays r JOIN tasks t ON t.id = r.task WHERE r.task = ?`, task).Scan(&owes)
	return owes, err
}

// Question is a question an agent asked under an open task that nothing has
// been filed after: nobody has answered it.
type Question struct {
	ID       int64
	Task     string
	Agent    string
	Assigner string
	Text     string
	At       time.Time
	// Escalated and Alerted are when it went to the orchestrator and when you
	// were told; zero before.
	Escalated time.Time
	Alerted   time.Time
}

// Unanswered is the questions asked under open tasks that nothing was filed
// after (an answer, a follow-up, the agent's own next word), oldest first.
// One still held for its assigner (not delivered yet) is not here: nobody
// has been asked, so its wait has not begun. A question that was held counts
// from when it was delivered (StampDelivered).
func (db *DB) Unanswered() ([]Question, error) {
	rows, err := db.sql.Query(`SELECT r.id, r.task, COALESCE(t.agent,''), COALESCE(t.assigner,''), COALESCE(r.text,''), COALESCE(r.delivered_at, r.at),
		r.escalated, r.alerted FROM relays r JOIN tasks t ON t.id = r.task
		WHERE r.kind=? AND r.sender = t.agent AND t.closed IS NULL
		AND (COALESCE(r.delivered,0) OR COALESCE(r.error,'') NOT IN (?, ?, ?))
		AND NOT EXISTS (SELECT 1 FROM relays l WHERE l.task = r.task AND l.id > r.id)
		ORDER BY r.id`, KindQuestion, Sending, Held, Interrupted)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Question
	for rows.Next() {
		var q Question
		var at int64
		var esc, al sql.NullInt64
		if err := rows.Scan(&q.ID, &q.Task, &q.Agent, &q.Assigner, &q.Text, &at, &esc, &al); err != nil {
			return nil, err
		}
		q.At, q.Escalated, q.Alerted = time.Unix(at, 0), fromUnix(esc), fromUnix(al)
		out = append(out, q)
	}
	return out, rows.Err()
}

// Cursor is the id of the last message of a task that a reader (a session's
// name) was handed by a wait --new; 0 if none.
func (db *DB) Cursor(task, reader string) (int64, error) {
	var seen int64
	err := db.sql.QueryRow(`SELECT seen FROM cursors WHERE task=? AND reader=?`, task, reader).Scan(&seen)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return seen, err
}

// SetCursor moves a reader's cursor on a task forward to id; never back.
func (db *DB) SetCursor(task, reader string, id int64) error {
	_, err := db.sql.Exec(`INSERT INTO cursors(task, reader, seen) VALUES(?,?,?)
		ON CONFLICT(task, reader) DO UPDATE SET seen=MAX(seen, excluded.seen)`, task, reader, id)
	return err
}

// Asking reports whether a task's agent has a question out that nothing was
// filed after (asking), and when its newest question was answered: the first
// message filed under the task after it (answeredAt; zero if none, or if
// the task has no question).
func (db *DB) Asking(task string) (asking bool, answeredAt time.Time, err error) {
	var q int64
	err = db.sql.QueryRow(`SELECT COALESCE(MAX(r.id),0) FROM relays r JOIN tasks t ON t.id=r.task
		WHERE r.task=? AND r.kind=? AND r.sender=t.agent`, task, KindQuestion).Scan(&q)
	if err != nil || q == 0 {
		return false, time.Time{}, err
	}
	var at sql.NullInt64
	err = db.sql.QueryRow(`SELECT at FROM relays WHERE task=? AND id>? ORDER BY id LIMIT 1`, task, q).Scan(&at)
	if errors.Is(err, sql.ErrNoRows) {
		return true, time.Time{}, nil
	}
	return false, fromUnix(at), err
}

// MarkEscalated records when a question went to the orchestrator.
func (db *DB) MarkEscalated(id int64, at time.Time) error {
	_, err := db.sql.Exec(`UPDATE relays SET escalated=? WHERE id=?`, at.Unix(), id)
	return err
}

// MarkAlerted records when a question was last the subject of an alert to you.
func (db *DB) MarkAlerted(id int64, at time.Time) error {
	_, err := db.sql.Exec(`UPDATE relays SET alerted=? WHERE id=?`, at.Unix(), id)
	return err
}

// LatestTask is an agent's most recently assigned task, nil if it has had none.
func (db *DB) LatestTask(agent string) (*Task, error) {
	ts, err := db.Tasks(false, agent, 1)
	if err != nil || len(ts) == 0 {
		return nil, err
	}
	return &ts[0], nil
}

func (db *DB) taskRelays(extra, task string, after int64) ([]Relay, error) {
	rows, err := db.sql.Query(`SELECT id, COALESCE(sender,''), COALESCE(recipient,''), at, COALESCE(text,''),
		COALESCE(delivered,0), COALESCE(error,''), COALESCE(kind,'report') FROM relays WHERE task=? AND id > ? `+extra+` ORDER BY id`, task, after)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Relay
	for rows.Next() {
		r := Relay{Task: task}
		var at int64
		if err := rows.Scan(&r.ID, &r.From, &r.To, &at, &r.Text, &r.Delivered, &r.Error, &r.Kind); err != nil {
			return nil, err
		}
		r.At = time.Unix(at, 0)
		r.Held = !r.Delivered && (r.Error == Held || r.Error == Interrupted)
		out = append(out, r)
	}
	return out, rows.Err()
}

// AddRelay records a relayed message.
func (db *DB) AddRelay(r Relay) error {
	_, err := db.addRelay(r)
	return err
}

func (db *DB) addRelay(r Relay) (int64, error) {
	if r.Kind == "" {
		r.Kind = KindReport
	}
	res, err := db.sql.Exec(`INSERT INTO relays(sender, recipient, at, size, delivered, text, error, task, kind) VALUES(?,?,?,?,?,?,?,?,?)`,
		r.From, r.To, r.At.Unix(), len(r.Text), r.Delivered, r.Text, nullStr(r.Error), nullStr(r.Task), r.Kind)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// A relay's error while it is on its way (Sending), once fleetd found it that
// way when it started (Interrupted: fleetd stopped in the middle of it), and
// while fleetd keeps it for a receiver it could not reach (Held).
const (
	Sending     = "sending"
	Interrupted = "interrupted"
	Held        = "held"
)

// BeginRelay records a message before it is sent, so a brief is never only
// in the memory of a fleetd that dies delivering it. EndRelay records how the
// send went.
func (db *DB) BeginRelay(r Relay) (int64, error) {
	r.Delivered, r.Error = false, Sending
	return db.addRelay(r)
}

// EndRelay records a send's outcome: delivered when err is nil.
func (db *DB) EndRelay(id int64, err error) error {
	var msg any
	if err != nil {
		msg = err.Error()
	}
	_, e := db.sql.Exec(`UPDATE relays SET delivered=?, error=? WHERE id=?`, err == nil, msg, id)
	return e
}

// StampDelivered records when a relay that was held got through, which is
// when a held question's wait for its answer begins.
func (db *DB) StampDelivered(id int64, at time.Time) error {
	_, err := db.sql.Exec(`UPDATE relays SET delivered_at=? WHERE id=?`, at.Unix(), id)
	return err
}

// HoldRelay keeps a relay that did not get through for sending later
// (Pending).
func (db *DB) HoldRelay(id int64) error {
	_, err := db.sql.Exec(`UPDATE relays SET delivered=0, error=? WHERE id=?`, Held, id)
	return err
}

// InterruptRelays marks the relays a previous fleetd left on their way as
// interrupted, and counts them. fleetd calls it once, when it starts.
func (db *DB) InterruptRelays() (int, error) {
	res, err := db.sql.Exec(`UPDATE relays SET error=? WHERE error=? AND NOT COALESCE(delivered,0)`, Interrupted, Sending)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	return int(n), err
}

// pendingWhere is what makes a relay still worth sending: no task, or a task
// still open whose receiver no later message got through to. A closed task's
// last word (the cancel notice, written as it closed) still goes.
const pendingWhere = `r.error IN (?, ?) AND (r.task IS NULL OR (
			EXISTS (SELECT 1 FROM tasks t WHERE t.id=r.task AND (t.closed IS NULL OR r.at >= t.closed))
			AND NOT EXISTS (SELECT 1 FROM relays l WHERE l.task=r.task AND l.recipient=r.recipient
				AND l.id > r.id AND COALESCE(l.delivered,0))))`

// Pending is the relays to the receiver since a time that are still to be
// sent — interrupted or held — and still matter (pendingWhere), oldest
// first. A zero since is all of them: a held message has no age limit.
func (db *DB) Pending(agent string, since time.Time) ([]Relay, error) {
	rows, err := db.sql.Query(`SELECT r.id, COALESCE(r.sender,''), r.at, COALESCE(r.text,''), COALESCE(r.task,''), r.error,
		COALESCE(r.alerted,0) FROM relays r WHERE r.recipient=? AND r.at >= ? AND `+pendingWhere+` ORDER BY r.id`,
		agent, since.Unix(), Interrupted, Held)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Relay
	for rows.Next() {
		r := Relay{To: agent, Held: true}
		var at int64
		if err := rows.Scan(&r.ID, &r.From, &at, &r.Text, &r.Task, &r.Error, &r.Alerted); err != nil {
			return nil, err
		}
		r.At = time.Unix(at, 0)
		out = append(out, r)
	}
	return out, rows.Err()
}

// HeldBefore is the pending relays, to any receiver, written before a time
// and not yet alerted on: the ones that have waited too long.
func (db *DB) HeldBefore(cut time.Time) ([]Relay, error) {
	rows, err := db.sql.Query(`SELECT r.id, COALESCE(r.sender,''), COALESCE(r.recipient,''), r.at, COALESCE(r.task,'')
		FROM relays r WHERE r.at < ? AND NOT COALESCE(r.alerted,0) AND `+pendingWhere+` ORDER BY r.id`,
		cut.Unix(), Interrupted, Held)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Relay
	for rows.Next() {
		r := Relay{Held: true}
		var at int64
		if err := rows.Scan(&r.ID, &r.From, &r.To, &at, &r.Task); err != nil {
			return nil, err
		}
		r.At = time.Unix(at, 0)
		out = append(out, r)
	}
	return out, rows.Err()
}

// AlertedRelay records that you were told a relay is held too long, so it
// is said once.
func (db *DB) AlertedRelay(id int64) error {
	_, err := db.sql.Exec(`UPDATE relays SET alerted=1 WHERE id=?`, id)
	return err
}

// HostSample is one container's resource use.
type HostSample struct {
	Container string
	At        time.Time
	CPU       float64 // percent
	MemBytes  int64
	MemPct    float64
	// From the streaming reader only; zero when docker did not say.
	MemLimit int64
	NetRx    int64
	NetTx    int64
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
	if _, err := tx.Exec(`DELETE FROM relays WHERE at < ? AND id NOT IN (SELECT r.id FROM relays r WHERE `+pendingWhere+`)`,
		now.Add(-r.Relays).Unix(), Interrupted, Held); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM host WHERE at < ?`, now.Add(-r.HostSamples).Unix()); err != nil {
		return err
	}
	return tx.Commit()
}
