package store

import (
	"database/sql"
	"errors"
	"net/url"
	"strings"

	"github.com/go-sql-driver/mysql"
)

// mysqlDialect covers MySQL and anything speaking its wire protocol — MariaDB,
// PlanetScale, Vitess.
//
// It is the dialect that diverges most. Three things in the canonical SQL have
// no MySQL spelling and are handled here: the upsert clause (ON CONFLICT vs ON
// DUPLICATE KEY), three column names that are reserved words, and TEXT columns
// used as keys, which MySQL cannot index without a length.
type mysqlDialect struct{}

var mysqlTypes = typeMap{
	text:     "LONGTEXT",
	keyText:  "VARCHAR(191)",
	integer:  "INT",
	bigint:   "BIGINT",
	datetime: "DATETIME(6)",
	real:     "DOUBLE",
	blob:     "LONGBLOB",
	autoInc:  "BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY",
	// The connection forces session time_zone to UTC, so CURRENT_TIMESTAMP is
	// the same instant SQLite's datetime('now') writes.
	nowDefault:  "DEFAULT CURRENT_TIMESTAMP(6)",
	tableSuffix: " ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci",
	// MySQL rejects IF NOT EXISTS on CREATE INDEX; Redundant() absorbs the
	// duplicate-key-name error instead.
	indexIfNotExists: false,
}

// mysqlReserved is deliberately only the words this schema actually uses as
// identifiers. A broader list would be worse, not safer: `words()` cannot tell
// a column named `order` from the ORDER in ORDER BY, so every entry added here
// is a way to corrupt a working query.
var mysqlReserved = map[string]bool{
	"key":    true, // kv_memory.key
	"loop":   true, // loop_runs.loop, loop_state.loop, loop_steps.loop, timers.loop
	"cursor": true, // connector_cursors.cursor
}

// unsupportedUpsert marks a statement MySQL cannot express. It is left in the
// SQL so the failure is loud and points at the cause; the one query that hits
// it (the loop lease) carries its own MySQL form instead.
const unsupportedUpsert = "/* karmax: conditional upsert has no MySQL form */"

func (mysqlDialect) Kind() Kind { return MySQL }

func (mysqlDialect) Rewrite(q string) string {
	q = replaceNow(q, "UTC_TIMESTAMP(6)")
	q = renameTwoArg(q, "MAX", "GREATEST")
	q = renameTwoArg(q, "MIN", "LEAST")
	q = rewriteDayString(q, func(a string) string { return "DATE_FORMAT(" + a + ", '%Y-%m-%d')" })
	q = mysqlCast(q)
	q = mysqlUpsert(q)
	q = mysqlInsertIgnore(q)
	return quoteIdents(q, mysqlReserved, '`', '`')
}

func (d mysqlDialect) DDL(stmt string, sch *schemaInfo) string {
	if !isSchemaStmt(stmt) {
		return d.Rewrite(stmt)
	}
	stmt = translateDDL(stmt, mysqlTypes, sch)
	stmt = mysqlUpsert(stmt)
	stmt = mysqlInsertIgnore(stmt)
	stmt = replaceNow(stmt, "UTC_TIMESTAMP(6)")
	return quoteIdents(stmt, mysqlReserved, '`', '`')
}

// Redundant matches "Duplicate column name" (1060), "Duplicate key name"
// (1061) and "Table already exists" (1050) — what re-running the schema on a
// live database produces.
func (mysqlDialect) Redundant(err error) bool {
	if err == nil {
		return false
	}
	var me *mysql.MySQLError
	if errors.As(err, &me) {
		switch me.Number {
		case 1050, 1060, 1061, 1826, 1091:
			return true
		}
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "duplicate column name") ||
		strings.Contains(msg, "duplicate key name") ||
		strings.Contains(msg, "already exists")
}

func (mysqlDialect) Tune(db *sql.DB) {
	db.SetMaxOpenConns(20)
	db.SetMaxIdleConns(5)
}

func (mysqlDialect) SerializeWrites() bool { return false }

func (mysqlDialect) ReturnsInsertID() bool { return true }

// mysqlCast maps SQLite's cast target names onto MySQL's.
func mysqlCast(q string) string {
	return mapCode(q, func(code string) string {
		up := strings.ToUpper(code)
		for from, to := range map[string]string{" AS INTEGER)": " AS SIGNED)", " AS REAL)": " AS DOUBLE)", " AS TEXT)": " AS CHAR)"} {
			for {
				i := strings.Index(up, from)
				if i < 0 {
					break
				}
				code = code[:i] + to + code[i+len(from):]
				up = strings.ToUpper(code)
			}
		}
		return code
	})
}

// mysqlInsertIgnore turns SQLite's INSERT OR IGNORE into MySQL's prefix form.
func mysqlInsertIgnore(q string) string {
	i := indexFold(q, "INSERT OR IGNORE")
	if i < 0 {
		return q
	}
	return q[:i] + "INSERT IGNORE" + q[i+len("INSERT OR IGNORE"):]
}

// mysqlUpsert rewrites the ON CONFLICT clause.
//
//	ON CONFLICT(a, b) DO NOTHING          -> INSERT IGNORE, clause dropped
//	ON CONFLICT(a) DO UPDATE SET x = ...  -> ON DUPLICATE KEY UPDATE x = ...
//
// Inside the assignment list, `excluded.x` — the row that failed to insert —
// becomes MySQL's VALUES(x). The VALUES() form is deprecated in 8.0.20 in
// favour of a row alias, but the alias breaks MariaDB and every MySQL before
// 8.0.19, so this stays on the form that works everywhere.
func mysqlUpsert(q string) string {
	mask := codeMask(q)
	at := findKeyword(mask, "ON", 0)
	for at >= 0 {
		next := skipSpace(mask, at+len("ON"))
		if strings.HasPrefix(mask[next:], "CONFLICT") {
			break
		}
		at = findKeyword(mask, "ON", at+len("ON"))
	}
	if at < 0 {
		return q
	}

	i := skipSpace(mask, at+len("ON"))
	i = skipSpace(mask, i+len("CONFLICT"))
	if i < len(mask) && mask[i] == '(' {
		i = matchParens(mask, i)
		i = skipSpace(mask, i)
	}
	if !strings.HasPrefix(mask[i:], "DO") {
		return q
	}
	i = skipSpace(mask, i+len("DO"))

	head, tail := q[:at], ""

	switch {
	case strings.HasPrefix(mask[i:], "NOTHING"):
		tail = q[i+len("NOTHING"):]
		return insertIgnoreVerb(strings.TrimRight(head, " \t\n") + tail)

	case strings.HasPrefix(mask[i:], "UPDATE"):
		j := skipSpace(mask, i+len("UPDATE"))
		if !strings.HasPrefix(mask[j:], "SET") {
			return q
		}
		j = skipSpace(mask, j+len("SET"))
		// A WHERE on the upsert branch makes this a compare-and-set, which
		// MySQL's ON DUPLICATE KEY UPDATE has no way to express.
		if w := findKeyword(mask, "WHERE", j); w >= 0 {
			return q + " " + unsupportedUpsert
		}
		assigns := excludedToValues(q[j:])
		return strings.TrimRight(head, " \t\n") + "\nON DUPLICATE KEY UPDATE " + assigns
	}
	return q
}

// insertIgnoreVerb turns the leading INSERT INTO into INSERT IGNORE INTO.
func insertIgnoreVerb(q string) string {
	i := indexFold(q, "INSERT")
	if i < 0 {
		return q
	}
	return q[:i] + "INSERT IGNORE" + q[i+len("INSERT"):]
}

// excludedToValues rewrites `excluded.col` into MySQL's VALUES(col).
func excludedToValues(assigns string) string {
	return mapCode(assigns, func(code string) string {
		var b strings.Builder
		for i := 0; i < len(code); {
			if !isIdentByte(code[i]) {
				b.WriteByte(code[i])
				i++
				continue
			}
			j := i
			for j < len(code) && isIdentByte(code[j]) {
				j++
			}
			w := code[i:j]
			if !strings.EqualFold(w, "excluded") || j >= len(code) || code[j] != '.' {
				b.WriteString(w)
				i = j
				continue
			}
			k := j + 1
			for k < len(code) && isIdentByte(code[k]) {
				k++
			}
			b.WriteString("VALUES(" + code[j+1:k] + ")")
			i = k
		}
		return b.String()
	})
}

// mysqlFromURL converts a mysql:// URL into the DSN go-sql-driver expects.
func mysqlFromURL(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", err
	}
	cfg := mysql.NewConfig()
	cfg.Net = "tcp"
	cfg.Addr = u.Host
	if u.User != nil {
		cfg.User = u.User.Username()
		cfg.Passwd, _ = u.User.Password()
	}
	cfg.DBName = strings.TrimPrefix(u.Path, "/")
	cfg.Params = map[string]string{}
	for k, v := range u.Query() {
		if len(v) > 0 {
			cfg.Params[k] = v[0]
		}
	}
	applyMySQLDefaults(cfg)
	return cfg.FormatDSN(), nil
}

// normaliseMySQLDSN applies the same defaults to a native go-sql-driver DSN.
func normaliseMySQLDSN(raw string) string {
	cfg, err := mysql.ParseDSN(raw)
	if err != nil {
		return raw
	}
	applyMySQLDefaults(cfg)
	return cfg.FormatDSN()
}

// applyMySQLDefaults pins the connection to UTC and asks the driver for
// time.Time values, so a DATETIME means the same instant it does under SQLite.
func applyMySQLDefaults(cfg *mysql.Config) {
	cfg.ParseTime = true
	cfg.Loc = utcLocation
	if cfg.Params == nil {
		cfg.Params = map[string]string{}
	}
	if _, ok := cfg.Params["time_zone"]; !ok {
		cfg.Params["time_zone"] = "'+00:00'"
	}
	cfg.MultiStatements = false
}
