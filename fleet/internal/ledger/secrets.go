package ledger

import (
	"database/sql"
	"errors"
	"time"
)

// Secrets are stored sealed (internal/vault): the ledger never holds a value
// in the clear. A grant names a task or an agent, never both; a task's grants
// go when it closes (CloseTask, SupersedeOpen), and one with an expiry goes
// when that passes.
const secretsSchema = `
CREATE TABLE IF NOT EXISTS secrets (
  name TEXT PRIMARY KEY, sealed BLOB NOT NULL, created INTEGER NOT NULL, updated INTEGER NOT NULL,
  read_at INTEGER, read_agent TEXT, read_task TEXT);
CREATE TABLE IF NOT EXISTS secret_grants (
  secret TEXT NOT NULL, task TEXT NOT NULL DEFAULT '', agent TEXT NOT NULL DEFAULT '',
  granted INTEGER NOT NULL, expires INTEGER, by TEXT,
  PRIMARY KEY (secret, task, agent));
`

// ErrNoSecret is a secret the ledger does not have.
var ErrNoSecret = errors.New("no such secret")

// Grant lets an agent, or whoever works a task, read a secret. Expires is
// zero for a grant with no TTL.
type Grant struct {
	Secret  string    `json:"-"`
	Task    string    `json:"task,omitempty"`
	Agent   string    `json:"agent,omitempty"`
	By      string    `json:"by,omitempty"`
	Granted time.Time `json:"granted"`
	Expires time.Time `json:"expires,omitzero"`
}

// SecretInfo is what may be said of a secret: never its value.
type SecretInfo struct {
	Name      string    `json:"name"`
	Created   time.Time `json:"created"`
	Updated   time.Time `json:"updated"`
	Grants    []Grant   `json:"grants"`
	ReadAt    time.Time `json:"read_at,omitzero"`
	ReadAgent string    `json:"read_agent,omitempty"`
	ReadTask  string    `json:"read_task,omitempty"`
}

// PutSecret stores a sealed value under a name, replacing any before it.
// Its grants stay.
func (db *DB) PutSecret(name string, sealed []byte, at time.Time) error {
	_, err := db.sql.Exec(`INSERT INTO secrets(name, sealed, created, updated) VALUES(?,?,?,?)
		ON CONFLICT(name) DO UPDATE SET sealed=excluded.sealed, updated=excluded.updated`,
		name, sealed, at.Unix(), at.Unix())
	return err
}

// SealedSecret is a secret's sealed value.
func (db *DB) SealedSecret(name string) ([]byte, error) {
	var b []byte
	err := db.sql.QueryRow(`SELECT sealed FROM secrets WHERE name=?`, name).Scan(&b)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNoSecret
	}
	return b, err
}

// SealedSecrets is every secret's sealed value, by name.
func (db *DB) SealedSecrets() (map[string][]byte, error) {
	rows, err := db.sql.Query(`SELECT name, sealed FROM secrets`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]byte{}
	for rows.Next() {
		var name string
		var b []byte
		if err := rows.Scan(&name, &b); err != nil {
			return nil, err
		}
		out[name] = b
	}
	return out, rows.Err()
}

// RemoveSecret drops a secret and its grants. false: there was none.
func (db *DB) RemoveSecret(name string) (bool, error) {
	if _, err := db.sql.Exec(`DELETE FROM secret_grants WHERE secret=?`, name); err != nil {
		return false, err
	}
	res, err := db.sql.Exec(`DELETE FROM secrets WHERE name=?`, name)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// AddGrant records a grant, replacing one to the same task or agent (a new
// TTL starts from now).
func (db *DB) AddGrant(g Grant) error {
	if _, err := db.SealedSecret(g.Secret); err != nil {
		return err
	}
	_, err := db.sql.Exec(`INSERT INTO secret_grants(secret, task, agent, granted, expires, by) VALUES(?,?,?,?,?,?)
		ON CONFLICT(secret, task, agent) DO UPDATE SET granted=excluded.granted, expires=excluded.expires, by=excluded.by`,
		g.Secret, g.Task, g.Agent, g.Granted.Unix(), unix(g.Expires), nullStr(g.By))
	return err
}

// Revoke drops the grant of a secret to a task or an agent. false: there
// was none.
func (db *DB) Revoke(secret, task, agent string) (bool, error) {
	res, err := db.sql.Exec(`DELETE FROM secret_grants WHERE secret=? AND task=? AND agent=?`, secret, task, agent)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// dropExpired forgets the grants whose TTL has passed.
func (db *DB) dropExpired(now time.Time) error {
	_, err := db.sql.Exec(`DELETE FROM secret_grants WHERE expires IS NOT NULL AND expires <= ?`, now.Unix())
	return err
}

// Granted reports whether a secret is granted, now, to the agent or to the
// task it works. The caller checks that the task is the agent's and open.
func (db *DB) Granted(secret, agent, task string, now time.Time) (bool, error) {
	if err := db.dropExpired(now); err != nil {
		return false, err
	}
	var n int
	err := db.sql.QueryRow(`SELECT count(*) FROM secret_grants WHERE secret=?
		AND ((task<>'' AND task=?) OR (agent<>'' AND agent=?))`, secret, task, agent).Scan(&n)
	return n > 0, err
}

// SecretRead records who last read a secret.
func (db *DB) SecretRead(secret, agent, task string, at time.Time) error {
	_, err := db.sql.Exec(`UPDATE secrets SET read_at=?, read_agent=?, read_task=? WHERE name=?`,
		at.Unix(), agent, nullStr(task), secret)
	return err
}

// Secrets is every secret's name, live grants and last read, by name.
func (db *DB) Secrets(now time.Time) ([]SecretInfo, error) {
	if err := db.dropExpired(now); err != nil {
		return nil, err
	}
	rows, err := db.sql.Query(`SELECT name, created, updated, read_at, COALESCE(read_agent,''), COALESCE(read_task,'')
		FROM secrets ORDER BY name`)
	if err != nil {
		return nil, err
	}
	out := []SecretInfo{}
	at := map[string]int{}
	for rows.Next() {
		s := SecretInfo{Grants: []Grant{}}
		var created, updated int64
		var read sql.NullInt64
		if err := rows.Scan(&s.Name, &created, &updated, &read, &s.ReadAgent, &s.ReadTask); err != nil {
			rows.Close()
			return nil, err
		}
		s.Created, s.Updated, s.ReadAt = time.Unix(created, 0).UTC(), time.Unix(updated, 0).UTC(), fromUnix(read)
		at[s.Name] = len(out)
		out = append(out, s)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	rows, err = db.sql.Query(`SELECT secret, task, agent, granted, expires, COALESCE(by,'') FROM secret_grants ORDER BY granted, task, agent`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var g Grant
		var granted int64
		var expires sql.NullInt64
		if err := rows.Scan(&g.Secret, &g.Task, &g.Agent, &granted, &expires, &g.By); err != nil {
			return nil, err
		}
		g.Granted, g.Expires = time.Unix(granted, 0).UTC(), fromUnix(expires)
		if i, ok := at[g.Secret]; ok {
			out[i].Grants = append(out[i].Grants, g)
		}
	}
	return out, rows.Err()
}

// endTaskGrants drops the grants to tasks that have closed.
func (db *DB) endTaskGrants(ids ...string) error {
	for _, id := range ids {
		if _, err := db.sql.Exec(`DELETE FROM secret_grants WHERE task=? AND task<>''`, id); err != nil {
			return err
		}
	}
	return nil
}
