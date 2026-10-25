package ledger

import (
	"database/sql"
	"errors"
	"strings"
	"time"

	"github.com/MelloB1989/karmax/fleet/internal/fleetapi"
	"github.com/MelloB1989/karmax/fleet/internal/tailnet"
)

// accessSchema is device access (internal/access): tagged devices, the
// agents lent to a guest device, and untagged devices that asked.
const accessSchema = `
CREATE TABLE IF NOT EXISTS devices (
  stable_id TEXT PRIMARY KEY, name TEXT NOT NULL, login TEXT NOT NULL, role TEXT NOT NULL,
  tagged_at INTEGER NOT NULL, tagged_by TEXT NOT NULL, last_seen INTEGER);
CREATE TABLE IF NOT EXISTS device_agents (
  stable_id TEXT NOT NULL, agent TEXT NOT NULL, PRIMARY KEY (stable_id, agent));
CREATE TABLE IF NOT EXISTS knocks (
  stable_id TEXT PRIMARY KEY, name TEXT NOT NULL, login TEXT NOT NULL,
  first_at INTEGER NOT NULL, last_at INTEGER NOT NULL, count INTEGER NOT NULL, told_at INTEGER);
`

// knockTold is how long after telling the operator of a device's knock its
// next knock goes untold.
const knockTold = 24 * time.Hour

// seenEvery is how stale a device's last_seen may get before a request
// writes it again: not a write per request.
const seenEvery = time.Minute

// Device is a tagged device by StableID.
func (db *DB) Device(stableID string) (fleetapi.Device, bool, error) {
	ds, err := db.devices(`WHERE stable_id=?`, stableID)
	if err != nil || len(ds) == 0 {
		return fleetapi.Device{}, false, err
	}
	return ds[0], true, nil
}

// Devices is every tagged device, by name.
func (db *DB) Devices() ([]fleetapi.Device, error) { return db.devices(`ORDER BY name, stable_id`) }

func (db *DB) devices(where string, args ...any) ([]fleetapi.Device, error) {
	rows, err := db.sql.Query(`SELECT stable_id, name, login, role, tagged_at, tagged_by, last_seen,
		(SELECT group_concat(agent, ',') FROM (SELECT agent FROM device_agents a WHERE a.stable_id=d.stable_id ORDER BY agent))
		FROM devices d `+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []fleetapi.Device
	for rows.Next() {
		var d fleetapi.Device
		var tagged int64
		var seen sql.NullInt64
		var agents sql.NullString
		if err := rows.Scan(&d.StableID, &d.Name, &d.Login, &d.Role, &tagged, &d.TaggedBy, &seen, &agents); err != nil {
			return nil, err
		}
		d.TaggedAt, d.LastSeen = time.Unix(tagged, 0).UTC(), fromUnix(seen)
		if agents.String != "" {
			d.Agents = strings.Split(agents.String, ",")
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// Tag sets a device's role and lent agents, replacing what it had, and
// drops its knock: it is no longer asking.
func (db *DB) Tag(d fleetapi.Device) error {
	if d.StableID == "" {
		return errors.New("a device tag needs its StableID")
	}
	tx, err := db.sql.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`INSERT INTO devices(stable_id, name, login, role, tagged_at, tagged_by) VALUES(?,?,?,?,?,?)
		ON CONFLICT(stable_id) DO UPDATE SET name=excluded.name, login=excluded.login, role=excluded.role,
		tagged_at=excluded.tagged_at, tagged_by=excluded.tagged_by`,
		d.StableID, d.Name, d.Login, string(d.Role), d.TaggedAt.Unix(), d.TaggedBy); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM device_agents WHERE stable_id=?`, d.StableID); err != nil {
		return err
	}
	for _, a := range d.Agents {
		if _, err := tx.Exec(`INSERT OR IGNORE INTO device_agents(stable_id, agent) VALUES(?,?)`, d.StableID, a); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`DELETE FROM knocks WHERE stable_id=?`, d.StableID); err != nil {
		return err
	}
	return tx.Commit()
}

// Untag removes a device's tag and its lends.
func (db *DB) Untag(stableID string) error {
	tx, err := db.sql.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM device_agents WHERE stable_id=?`, stableID); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM devices WHERE stable_id=?`, stableID); err != nil {
		return err
	}
	return tx.Commit()
}

// Seen marks a tagged device as last seen now (at most once a minute).
func (db *DB) Seen(stableID string, now time.Time) error {
	_, err := db.sql.Exec(`UPDATE devices SET last_seen=? WHERE stable_id=? AND (last_seen IS NULL OR last_seen<=?)`,
		now.Unix(), stableID, now.Add(-seenEvery).Unix())
	return err
}

// Knock records that an untagged device asked. first is whether the operator
// should be told: no knock of this device was told in the last 24 hours.
func (db *DB) Knock(w tailnet.Who, now time.Time) (first bool, err error) {
	tx, err := db.sql.Begin()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var told sql.NullInt64
	err = tx.QueryRow(`SELECT told_at FROM knocks WHERE stable_id=?`, w.StableID).Scan(&told)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		first = true
	case err != nil:
		return false, err
	default:
		first = !told.Valid || now.Sub(fromUnix(told)) >= knockTold
	}
	if _, err := tx.Exec(`INSERT INTO knocks(stable_id, name, login, first_at, last_at, count) VALUES(?,?,?,?,?,1)
		ON CONFLICT(stable_id) DO UPDATE SET name=excluded.name, login=excluded.login, last_at=excluded.last_at, count=count+1`,
		w.StableID, w.Node, w.Login, now.Unix(), now.Unix()); err != nil {
		return false, err
	}
	if first {
		if _, err := tx.Exec(`UPDATE knocks SET told_at=? WHERE stable_id=?`, now.Unix(), w.StableID); err != nil {
			return false, err
		}
	}
	return first, tx.Commit()
}

// Knocks is the latest knocks, newest first; limit 0 is all.
func (db *DB) Knocks(limit int) ([]fleetapi.Knock, error) {
	q := `SELECT stable_id, name, login, first_at, last_at, count FROM knocks ORDER BY last_at DESC, stable_id`
	args := []any{}
	if limit > 0 {
		q += ` LIMIT ?`
		args = append(args, limit)
	}
	rows, err := db.sql.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []fleetapi.Knock
	for rows.Next() {
		var k fleetapi.Knock
		var first, last int64
		if err := rows.Scan(&k.StableID, &k.Name, &k.Login, &first, &last, &k.Count); err != nil {
			return nil, err
		}
		k.First, k.Last = time.Unix(first, 0).UTC(), time.Unix(last, 0).UTC()
		out = append(out, k)
	}
	return out, rows.Err()
}
