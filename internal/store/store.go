package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	_ "github.com/go-sql-driver/mysql"
	_ "github.com/jackc/pgx/v5/stdlib"
	_ "github.com/mattn/go-sqlite3"
	"go.uber.org/zap"
)

var utcLocation = time.UTC

// Store is KARMAX's durable state, on whichever database the operator pointed
// it at.
//
// Every query in this package is written once, in SQLite's dialect, and
// translated on the way out — see dialect.go. Nothing above this package knows
// or needs to know which backend is underneath.
type Store struct {
	db  *sql.DB
	d   Dialect
	log *zap.Logger
	mu  gate

	// cache holds the translated form of each statement. Translation is pure
	// text work on a fixed set of literals, so it is done once per statement
	// rather than once per call.
	cache sync.Map
}

// New opens the store.
//
// dsn is either a filesystem path — every existing install, and still the
// default — or a connection URL:
//
//	/home/me/.karmax/db/karmax.db
//	postgres://user:pass@host:5432/karmax?sslmode=require
//	mysql://user:pass@host:3306/karmax
func New(dsn string, log *zap.Logger) (*Store, error) {
	target, err := ParseDSN(dsn)
	if err != nil {
		return nil, err
	}

	d, err := dialectFor(target.Kind)
	if err != nil {
		return nil, err
	}

	db, err := sql.Open(target.Driver, target.Conn)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", target.Kind, err)
	}
	d.Tune(db)

	s := &Store{db: db, d: d, log: log}
	s.mu.on = d.SerializeWrites()

	// A filesystem path is open even when the file is unwritable; a network
	// database is not open until it answers. Fail here rather than on the
	// first query, so a bad DSN is a startup error.
	if target.Kind != SQLite {
		if err := db.Ping(); err != nil {
			db.Close()
			return nil, fmt.Errorf("connect %s: %w", target.Kind, err)
		}
	}

	if err := s.migrate(); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrations: %w", err)
	}

	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) DB() *sql.DB { return s.db }

// Kind reports the backend in use. Store methods branch on it only where a
// statement genuinely has no portable form.
func (s *Store) Kind() Kind { return s.d.Kind() }

// sqlFor translates a canonical statement, memoised.
func (s *Store) sqlFor(q string) string {
	if v, ok := s.cache.Load(q); ok {
		return v.(string)
	}
	out := s.d.Rewrite(q)
	s.cache.Store(q, out)
	return out
}

func (s *Store) exec(q string, args ...any) (sql.Result, error) {
	return s.db.Exec(s.sqlFor(q), utcArgs(args)...)
}

func (s *Store) query(q string, args ...any) (*sql.Rows, error) {
	return s.db.Query(s.sqlFor(q), utcArgs(args)...)
}

func (s *Store) queryRow(q string, args ...any) *sql.Row {
	return s.db.QueryRow(s.sqlFor(q), utcArgs(args)...)
}

func (s *Store) prepare(q string) (*sql.Stmt, error) {
	return s.db.Prepare(s.sqlFor(q))
}

// insertReturningID runs an INSERT and returns the generated key, plus whether
// a row was actually written.
//
// Postgres has no LastInsertId, so the column is read back with RETURNING.
// The inserted flag is what makes this usable with ON CONFLICT DO NOTHING: on a
// conflict SQLite's LastInsertId still reports the PREVIOUS insert's rowid, so
// the caller has to be told nothing happened rather than handed a stale id.
func (s *Store) insertReturningID(q, idColumn string, args ...any) (id int64, inserted bool, err error) {
	args = utcArgs(args)

	if s.d.ReturnsInsertID() {
		res, err := s.db.Exec(s.sqlFor(q), args...)
		if err != nil {
			return 0, false, err
		}
		if n, err := res.RowsAffected(); err == nil && n == 0 {
			return 0, false, nil
		}
		id, err = res.LastInsertId()
		return id, err == nil, err
	}

	stmt := strings.TrimRight(s.sqlFor(q), " \t\n;") + " RETURNING " + idColumn
	switch err := s.db.QueryRow(stmt, args...).Scan(&id); {
	case err == sql.ErrNoRows:
		return 0, false, nil
	case err != nil:
		return 0, false, err
	}
	return id, true, nil
}

// Tx is a transaction that translates its statements the same way the Store
// does.
type Tx struct {
	tx *sql.Tx
	s  *Store
}

func (s *Store) begin() (*Tx, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	return &Tx{tx: tx, s: s}, nil
}

func (t *Tx) Exec(q string, args ...any) (sql.Result, error) {
	return t.tx.Exec(t.s.sqlFor(q), utcArgs(args)...)
}

func (t *Tx) Query(q string, args ...any) (*sql.Rows, error) {
	return t.tx.Query(t.s.sqlFor(q), utcArgs(args)...)
}

func (t *Tx) QueryRow(q string, args ...any) *sql.Row {
	return t.tx.QueryRow(t.s.sqlFor(q), utcArgs(args)...)
}

func (t *Tx) Prepare(q string) (*Stmt, error) {
	st, err := t.tx.Prepare(t.s.sqlFor(q))
	if err != nil {
		return nil, err
	}
	return &Stmt{stmt: st}, nil
}

func (t *Tx) Commit() error   { return t.tx.Commit() }
func (t *Tx) Rollback() error { return t.tx.Rollback() }

// Stmt is a prepared statement whose arguments are normalised the same way a
// direct call's are.
type Stmt struct{ stmt *sql.Stmt }

func (s *Stmt) Exec(args ...any) (sql.Result, error) {
	return s.stmt.Exec(utcArgs(args)...)
}

func (s *Stmt) Query(args ...any) (*sql.Rows, error) {
	return s.stmt.Query(utcArgs(args)...)
}

func (s *Stmt) QueryRow(args ...any) *sql.Row {
	return s.stmt.QueryRow(utcArgs(args)...)
}

func (s *Stmt) Close() error { return s.stmt.Close() }

// errorAs is errors.As, wrapped so dialect files need not import errors for a
// single call.
func errorAs(err error, target any) bool { return errors.As(err, target) }

// Ping opens the target, checks it answers, and closes it — without running
// migrations. It is what `karmax doctor` needs: a bad DSN or an unreachable
// server should be a line in the diagnostic rather than a failed daemon start.
func Ping(dsn string, timeout time.Duration) error {
	target, err := ParseDSN(dsn)
	if err != nil {
		return err
	}
	db, err := sql.Open(target.Driver, target.Conn)
	if err != nil {
		return err
	}
	defer db.Close()

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return db.PingContext(ctx)
}
