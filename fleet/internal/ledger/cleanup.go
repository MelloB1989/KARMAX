package ledger

import (
	"strings"
	"time"
)

// cleanupSchema is what the cleanup told the operator it kept (work found
// nowhere else), so each item is told once.
const cleanupSchema = `
CREATE TABLE IF NOT EXISTS cleanup_kept (
  place TEXT NOT NULL, path TEXT NOT NULL, told INTEGER NOT NULL, PRIMARY KEY (place, path));
`

// KeptTold records that the operator is told the cleanup kept path in place
// (an agent's container, the shared code), and reports whether this is the
// first time.
func (db *DB) KeptTold(place, path string, now time.Time) (bool, error) {
	res, err := db.sql.Exec(`INSERT OR IGNORE INTO cleanup_kept(place, path, told) VALUES(?,?,?)`, place, path, now.Unix())
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// ForgetKept forgets what a pass over place no longer keeps: deleted since,
// or by hand. If it comes back and is kept again, it is told again.
func (db *DB) ForgetKept(place string, still []string) error {
	q := `DELETE FROM cleanup_kept WHERE place=?`
	args := []any{place}
	if len(still) > 0 {
		q += ` AND path NOT IN (?` + strings.Repeat(",?", len(still)-1) + `)`
		for _, p := range still {
			args = append(args, p)
		}
	}
	_, err := db.sql.Exec(q, args...)
	return err
}
