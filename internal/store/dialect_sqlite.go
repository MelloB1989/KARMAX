package store

import (
	"database/sql"
	"strings"
)

// sqliteDialect is the identity translation: the canonical SQL in this package
// is already SQLite's, so nothing an existing install runs changes shape.
type sqliteDialect struct{}

var sqliteTypes = typeMap{
	text:             "TEXT",
	keyText:          "TEXT",
	integer:          "INTEGER",
	bigint:           "INTEGER",
	datetime:         "DATETIME",
	real:             "REAL",
	blob:             "BLOB",
	autoInc:          "",
	nowDefault:       "DEFAULT (datetime('now'))",
	indexIfNotExists: true,
}

func (sqliteDialect) Kind() Kind { return SQLite }

func (sqliteDialect) Rewrite(q string) string { return q }

func (sqliteDialect) DDL(stmt string, sch *schemaInfo) string {
	return translateDDL(stmt, sqliteTypes, sch)
}

func (sqliteDialect) Redundant(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "duplicate column name")
}

// Tune pins SQLite to one connection. WAL allows concurrent readers, but a
// second writer is SQLITE_BUSY, and this is cheaper than retrying it.
func (sqliteDialect) Tune(db *sql.DB) { db.SetMaxOpenConns(1) }

func (sqliteDialect) SerializeWrites() bool { return true }

func (sqliteDialect) ReturnsInsertID() bool { return true }

// sqliteConn appends the pragmas KARMAX depends on: WAL for concurrent reads, a
// busy timeout so a contended write waits instead of failing, and foreign keys
// on. Left alone if the caller already supplied query parameters.
func sqliteConn(path string) string {
	if strings.Contains(path, "?") {
		return path
	}
	return path + "?_journal_mode=WAL&_busy_timeout=5000&_foreign_keys=on"
}
