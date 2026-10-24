package ledger

import (
	"database/sql"
	"encoding/json"
	"time"
)

// An approval's states. A request is pending until the orchestrator approves
// or denies it, or it expires; an approved one is ran once its agent has run
// it (Note says how that went).
const (
	Pending  = "pending"
	Approved = "approved"
	Denied   = "denied"
	Expired  = "expired"
	Ran      = "ran"
)

// Approval is one write an agent asked to make through the KARMAX gateway.
type Approval struct {
	ID     int64           `json:"id"`
	Agent  string          `json:"agent"`
	Task   string          `json:"task,omitempty"`
	Tool   string          `json:"tool"`
	Args   json.RawMessage `json:"args"`
	Reason string          `json:"reason"`
	State  string          `json:"state"`
	// DecidedBy is who approved or denied it; Note is the reason given for a
	// deny, or how the run went.
	DecidedBy string    `json:"decided_by,omitempty"`
	Note      string    `json:"note,omitempty"`
	At        time.Time `json:"at"`
	Decided   time.Time `json:"decided,omitzero"`
	Expires   time.Time `json:"expires"`
}

// AddApproval files a pending request and returns its id.
func (db *DB) AddApproval(a Approval) (int64, error) {
	args := string(a.Args)
	if args == "" {
		args = "{}"
	}
	res, err := db.sql.Exec(`INSERT INTO approvals(agent, task, tool, args, reason, state, at, expires) VALUES(?,?,?,?,?,?,?,?)`,
		a.Agent, nullStr(a.Task), a.Tool, args, a.Reason, Pending, a.At.Unix(), a.Expires.Unix())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

const approvalCols = `id, agent, COALESCE(task,''), tool, COALESCE(args,'{}'), COALESCE(reason,''), state,
	COALESCE(decided_by,''), COALESCE(note,''), at, decided, expires`

func scanApproval(row interface{ Scan(...any) error }) (Approval, error) {
	var a Approval
	var args string
	var at, expires int64
	var decided sql.NullInt64
	err := row.Scan(&a.ID, &a.Agent, &a.Task, &a.Tool, &args, &a.Reason, &a.State, &a.DecidedBy, &a.Note, &at, &decided, &expires)
	a.Args = json.RawMessage(args)
	a.At, a.Decided, a.Expires = time.Unix(at, 0).UTC(), fromUnix(decided), time.Unix(expires, 0).UTC()
	return a, err
}

// GetApproval reads one request; sql.ErrNoRows when there is none.
func (db *DB) GetApproval(id int64) (Approval, error) {
	return scanApproval(db.sql.QueryRow(`SELECT `+approvalCols+` FROM approvals WHERE id=?`, id))
}

// Approvals lists requests in one state (every state when empty), oldest
// first; limit <= 0 means all.
func (db *DB) Approvals(state string, limit int) ([]Approval, error) {
	q := `SELECT ` + approvalCols + ` FROM approvals WHERE (?='' OR state=?) ORDER BY id`
	args := []any{state, state}
	if limit > 0 {
		q = `SELECT * FROM (SELECT ` + approvalCols + ` FROM approvals WHERE (?='' OR state=?) ORDER BY id DESC LIMIT ?) ORDER BY id`
		args = append(args, limit)
	}
	rows, err := db.sql.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Approval{}
	for rows.Next() {
		a, err := scanApproval(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// MoveApproval moves a request from one state to another, and reports
// whether it did: only one of two callers racing (an approve and an expiry,
// two waiters) wins. by and note are kept when empty.
func (db *DB) MoveApproval(id int64, from, to, by, note string, at time.Time) (bool, error) {
	res, err := db.sql.Exec(`UPDATE approvals SET state=?, decided_by=COALESCE(?, decided_by), note=COALESCE(?, note),
		decided=COALESCE(decided, ?) WHERE id=? AND state=?`, to, nullStr(by), nullStr(note), at.Unix(), id, from)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// ExpireApprovals moves every pending request past its expiry to expired,
// and returns them.
func (db *DB) ExpireApprovals(now time.Time) ([]Approval, error) {
	rows, err := db.sql.Query(`SELECT `+approvalCols+` FROM approvals WHERE state=? AND expires <= ?`, Pending, now.Unix())
	if err != nil {
		return nil, err
	}
	var due []Approval
	for rows.Next() {
		a, err := scanApproval(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		due = append(due, a)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	var out []Approval
	for _, a := range due {
		ok, err := db.MoveApproval(a.ID, Pending, Expired, "", "", now)
		if err != nil {
			return out, err
		}
		if ok {
			a.State = Expired
			out = append(out, a)
		}
	}
	return out, nil
}
