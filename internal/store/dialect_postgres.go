package store

import (
	"database/sql"
	"net/url"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"
)

// postgresDialect covers every hosted Postgres — RDS, Cloud SQL, Neon,
// Supabase — through pgx's database/sql driver.
type postgresDialect struct{}

var postgresTypes = typeMap{
	text:     "TEXT",
	keyText:  "TEXT",
	integer:  "INTEGER",
	bigint:   "BIGINT",
	datetime: "TIMESTAMP",
	real:     "DOUBLE PRECISION",
	blob:     "BYTEA",
	autoInc:  "BIGSERIAL PRIMARY KEY",
	// Stored UTC, to match what SQLite's datetime('now') has always written.
	nowDefault:       "DEFAULT (now() AT TIME ZONE 'utc')",
	indexIfNotExists: true,
}

func (postgresDialect) Kind() Kind { return Postgres }

func (postgresDialect) Rewrite(q string) string {
	q = replaceNow(q, "(now() AT TIME ZONE 'utc')")
	// SQLite's two-argument MAX/MIN is the scalar form, which Postgres spells
	// GREATEST/LEAST and would otherwise reject as an aggregate misuse.
	q = renameTwoArg(q, "MAX", "GREATEST")
	q = renameTwoArg(q, "MIN", "LEAST")
	q = rewriteDayString(q, func(a string) string { return "to_char(" + a + ", 'YYYY-MM-DD')" })
	// SQLite's LIKE ignores ASCII case and Postgres's does not, so a search
	// that matched before the move must keep matching after it.
	q = mapCode(q, func(code string) string {
		return words(code, func(w, _ string) string {
			if strings.EqualFold(w, "LIKE") {
				return "ILIKE"
			}
			return w
		})
	})
	q = pgInsertIgnore(q)
	return rebind(q)
}

func (d postgresDialect) DDL(stmt string, sch *schemaInfo) string {
	if !isSchemaStmt(stmt) {
		return d.Rewrite(stmt)
	}
	return translateDDL(stmt, postgresTypes, sch)
}

// Redundant matches the SQLSTATEs for "column already exists" (42701),
// "duplicate object" (42710) and "duplicate table" (42P07) — what a re-run of
// the schema produces.
func (postgresDialect) Redundant(err error) bool {
	if err == nil {
		return false
	}
	var pge *pgconn.PgError
	if errorAs(err, &pge) {
		switch pge.Code {
		case "42701", "42710", "42P07", "42P16":
			return true
		}
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "already exists")
}

func (postgresDialect) Tune(db *sql.DB) {
	db.SetMaxOpenConns(20)
	db.SetMaxIdleConns(5)
}

func (postgresDialect) SerializeWrites() bool { return false }

// ReturnsInsertID is false: Postgres has no LastInsertId, so a generated key is
// read back with RETURNING.
func (postgresDialect) ReturnsInsertID() bool { return false }

// pgInsertIgnore turns SQLite's INSERT OR IGNORE into the ON CONFLICT form.
func pgInsertIgnore(q string) string {
	i := indexFold(q, "INSERT OR IGNORE")
	if i < 0 {
		return q
	}
	q = q[:i] + "INSERT" + q[i+len("INSERT OR IGNORE"):]
	if indexFold(q, "ON CONFLICT") >= 0 {
		return q
	}
	return strings.TrimRight(q, " \t\n;") + " ON CONFLICT DO NOTHING"
}

// normalisePostgres forces UTC so a timestamp column means the same instant it
// does under SQLite, whatever zone the server or the daemon runs in.
func normalisePostgres(dsn string) string {
	u, err := url.Parse(dsn)
	if err != nil {
		return dsn
	}
	q := u.Query()
	if q.Get("TimeZone") == "" && q.Get("timezone") == "" {
		q.Set("TimeZone", "UTC")
	}
	u.RawQuery = q.Encode()
	return u.String()
}
