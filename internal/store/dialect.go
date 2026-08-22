package store

import (
	"database/sql"
	"fmt"
	"strings"
	"sync"
	"time"
)

// Kind names a supported backend.
type Kind string

const (
	SQLite   Kind = "sqlite"
	Postgres Kind = "postgres"
	MySQL    Kind = "mysql"
)

// Dialect turns KARMAX's canonical SQL into what one backend accepts.
//
// The canonical form is SQLite's, because that is what every query in this
// package was already written in: `?` placeholders, `datetime('now')`,
// `ON CONFLICT ... DO UPDATE SET x = excluded.x`. Nothing outside this file and
// its siblings knows which backend is underneath — a store method writes the
// one dialect and the Store rewrites it on the way to the driver.
type Dialect interface {
	Kind() Kind

	// Rewrite translates one canonical DML statement. Called once per distinct
	// statement; the result is cached by the Store.
	Rewrite(q string) string

	// DDL translates one canonical schema statement. An empty result means the
	// statement does not apply to this backend and should be skipped.
	DDL(stmt string, schema *schemaInfo) string

	// Redundant reports whether an error from a schema statement is the
	// backend's way of saying "already done" — a re-added column, a re-created
	// index. Migrations run on every boot, so these are expected.
	Redundant(err error) bool

	// Tune sizes the connection pool.
	Tune(db *sql.DB)

	// SerializeWrites is true where one writer at a time is a hard requirement
	// of the engine rather than a throughput choice.
	SerializeWrites() bool

	// ReturnsInsertID is false where LastInsertId is unsupported and the
	// generated key has to be read back with RETURNING.
	ReturnsInsertID() bool
}

// DSN is a parsed connection target.
type DSN struct {
	Kind Kind
	// Conn is what gets handed to the driver, already normalised (UTC time
	// handling, the SQLite pragmas KARMAX depends on).
	Conn string
	// Driver is the database/sql driver name.
	Driver string
}

// ParseDSN turns a user-supplied connection string into a backend choice.
//
// A bare filesystem path is SQLite, which is what every existing install has in
// its config, so nothing has to change for them.
func ParseDSN(raw string) (DSN, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return DSN{}, fmt.Errorf("empty database dsn")
	}

	switch {
	case hasPrefixFold(s, "postgres://"), hasPrefixFold(s, "postgresql://"):
		return DSN{Kind: Postgres, Driver: "pgx", Conn: normalisePostgres(s)}, nil

	case hasPrefixFold(s, "mysql://"):
		conn, err := mysqlFromURL(s)
		if err != nil {
			return DSN{}, err
		}
		return DSN{Kind: MySQL, Driver: "mysql", Conn: conn}, nil

	case hasPrefixFold(s, "sqlite://"), hasPrefixFold(s, "sqlite3://"):
		path := s[strings.Index(s, "://")+3:]
		return DSN{Kind: SQLite, Driver: "sqlite3", Conn: sqliteConn(path)}, nil

	case hasPrefixFold(s, "file:"):
		return DSN{Kind: SQLite, Driver: "sqlite3", Conn: s}, nil

	// A DSN in go-sql-driver form: user:pass@tcp(host:port)/db
	case strings.Contains(s, "@tcp(") || strings.Contains(s, "@unix("):
		return DSN{Kind: MySQL, Driver: "mysql", Conn: normaliseMySQLDSN(s)}, nil

	// Postgres keyword/value form: host=... dbname=...
	case strings.Contains(s, "host=") && strings.Contains(s, "="):
		return DSN{Kind: Postgres, Driver: "pgx", Conn: s}, nil

	default:
		// A scheme we do not know is a typo, not a filename. Treating it as one
		// would quietly create a file called "postgress:" and report success.
		if i := strings.Index(s, "://"); i > 0 {
			return DSN{}, fmt.Errorf("unsupported database scheme %q: want postgres://, mysql://, sqlite:// or a file path", s[:i])
		}
		return DSN{Kind: SQLite, Driver: "sqlite3", Conn: sqliteConn(s)}, nil
	}
}

func hasPrefixFold(s, prefix string) bool {
	return len(s) >= len(prefix) && strings.EqualFold(s[:len(prefix)], prefix)
}

// dialectFor returns the translator for a backend.
func dialectFor(k Kind) (Dialect, error) {
	switch k {
	case SQLite:
		return sqliteDialect{}, nil
	case Postgres:
		return postgresDialect{}, nil
	case MySQL:
		return mysqlDialect{}, nil
	default:
		return nil, fmt.Errorf("unsupported database %q", k)
	}
}

// gate is the write lock SQLite needs and a server database does not.
//
// It keeps the shape of a sync.RWMutex so the ~200 Lock/RLock pairs in this
// package are unchanged, and costs an atomic read when it is off. Serialising
// every statement through one mutex is correct against Postgres too, but it
// turns each network round trip into a queue for the whole process.
type gate struct {
	mu sync.RWMutex
	on bool
}

func (g *gate) Lock() {
	if g.on {
		g.mu.Lock()
	}
}

func (g *gate) Unlock() {
	if g.on {
		g.mu.Unlock()
	}
}

func (g *gate) RLock() {
	if g.on {
		g.mu.RLock()
	}
}

func (g *gate) RUnlock() {
	if g.on {
		g.mu.RUnlock()
	}
}

// utcArgs normalises time arguments so every backend stores the same instant.
//
// SQLite's driver converts a time.Time to UTC before writing it, and
// `datetime('now')` is UTC — so every timestamp already in a KARMAX database is
// UTC. Postgres `timestamp` and MySQL `datetime` have no zone and would
// otherwise keep whatever offset the process happened to be running in.
func utcArgs(args []any) []any {
	for i, a := range args {
		switch v := a.(type) {
		case time.Time:
			args[i] = v.UTC()
		case *time.Time:
			if v != nil {
				u := v.UTC()
				args[i] = u
			}
		}
	}
	return args
}
