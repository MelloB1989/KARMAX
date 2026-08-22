package store

import (
	"strings"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
)

// Pure unit tests for the dialect-translation layer: no database is opened
// anywhere in this file. Every check works on the text a Dialect produces.

// normSQL collapses all whitespace runs to a single space and trims the
// ends, so a comparison asserts on SQL meaning rather than on the column
// alignment / indentation migrations.go uses for readability.
func normSQL(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// findMigration returns the one statement in `migrations` whose (whitespace
// normalised) text starts with prefix. Failing the test if it can't find
// exactly one keeps these tests honest against migrations.go drifting.
func findMigration(t *testing.T, prefix string) string {
	t.Helper()
	prefix = normSQL(prefix)
	var found []string
	for _, m := range migrations {
		if strings.HasPrefix(normSQL(m), prefix) {
			found = append(found, m)
		}
	}
	if len(found) != 1 {
		t.Fatalf("expected exactly one migration starting with %q, found %d", prefix, len(found))
	}
	return found[0]
}

// ---------------------------------------------------------------------------
// A. Placeholder rebinding.

func TestRebindRenumbersOnlyCodePlaceholders(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "renumbers in order, skips a ? inside a string literal",
			in:   `SELECT a FROM t WHERE a = ? AND b LIKE '%?%'`,
			want: `SELECT a FROM t WHERE a = $1 AND b LIKE '%?%'`,
		},
		{
			name: "multiple placeholders number sequentially",
			in:   `INSERT INTO t (a,b,c) VALUES (?, ?, ?)`,
			want: `INSERT INTO t (a,b,c) VALUES ($1, $2, $3)`,
		},
		{
			name: "a ? inside a line comment is left alone",
			in:   "SELECT ?, ? -- what about this one: ?\n",
			want: "SELECT $1, $2 -- what about this one: ?\n",
		},
		{
			name: "a ? inside a block comment is left alone",
			in:   `SELECT ? /* mid ? comment */, ?`,
			want: `SELECT $1 /* mid ? comment */, $2`,
		},
		{
			name: "a ? inside a double-quoted identifier is left alone",
			in:   `SELECT "weird?col" FROM t WHERE a = ?`,
			want: `SELECT "weird?col" FROM t WHERE a = $1`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := rebind(tc.in)
			if got != tc.want {
				t.Errorf("rebind(%q)\n got:  %q\n want: %q", tc.in, got, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// B. datetime('now') substitution.

func TestReplaceNowSubstitutesOnlyTheFunctionCall(t *testing.T) {
	const pgNow = "(now() AT TIME ZONE 'utc')"
	const myNow = "UTC_TIMESTAMP(6)"

	tests := []struct {
		name string
		now  string
		in   string
		want string
	}{
		{
			name: "postgres form",
			now:  pgNow,
			in:   `INSERT INTO t (a, created_at) VALUES (?, datetime('now'))`,
			want: `INSERT INTO t (a, created_at) VALUES (?, (now() AT TIME ZONE 'utc'))`,
		},
		{
			name: "mysql form",
			now:  myNow,
			in:   `INSERT INTO t (a, created_at) VALUES (?, datetime('now'))`,
			want: `INSERT INTO t (a, created_at) VALUES (?, UTC_TIMESTAMP(6))`,
		},
		{
			name: "the bare string literal 'now' is not the function call and must survive",
			now:  pgNow,
			in:   `UPDATE t SET note = 'now', updated_at = datetime('now')`,
			want: `UPDATE t SET note = 'now', updated_at = (now() AT TIME ZONE 'utc')`,
		},
		{
			name: "case-insensitive DATETIME('NOW') still matches",
			now:  myNow,
			in:   `SELECT DATETIME('NOW')`,
			want: `SELECT UTC_TIMESTAMP(6)`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := replaceNow(tc.in, tc.now)
			if got != tc.want {
				t.Errorf("replaceNow(%q, %q)\n got:  %q\n want: %q", tc.in, tc.now, got, tc.want)
			}
		})
	}
}

func TestReplaceNowLeavesSQLiteAlone(t *testing.T) {
	q := `INSERT INTO t (created_at) VALUES (datetime('now'))`
	got := sqliteDialect{}.Rewrite(q)
	if got != q {
		t.Errorf("sqliteDialect.Rewrite must be identity, got %q want %q", got, q)
	}
}

// ---------------------------------------------------------------------------
// C. Two-arg MAX/MIN -> GREATEST/LEAST.

func TestRenameTwoArgOnlyRewritesTheScalarForm(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "two-argument MAX becomes GREATEST",
			in:   `SELECT MAX(a, b) FROM t`,
			want: `SELECT GREATEST(a, b) FROM t`,
		},
		{
			name: "single-argument aggregate MAX is untouched",
			in:   `SELECT MAX(seq) FROM event_log WHERE workspace = ?`,
			want: `SELECT MAX(seq) FROM event_log WHERE workspace = ?`,
		},
		{
			name: "the real event_offsets upsert query from event_log_store.go",
			in: `ON CONFLICT(subscriber, workspace) DO UPDATE SET
  seq = MAX(event_offsets.seq, excluded.seq),
  updated_at = excluded.updated_at`,
			want: `ON CONFLICT(subscriber, workspace) DO UPDATE SET
  seq = GREATEST(event_offsets.seq, excluded.seq),
  updated_at = excluded.updated_at`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := renameTwoArg(tc.in, "MAX", "GREATEST")
			if normSQL(got) != normSQL(tc.want) {
				t.Errorf("renameTwoArg(%q)\n got:  %q\n want: %q", tc.in, got, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// D. MySQL upsert rewrite.

func TestMySQLUpsertRewrite(t *testing.T) {
	t.Run("DO UPDATE SET becomes ON DUPLICATE KEY UPDATE, excluded.x becomes VALUES(x)", func(t *testing.T) {
		in := `INSERT INTO t (id,a,b) VALUES (?,?,?) ON CONFLICT(id) DO UPDATE SET a = excluded.a, b = t.b + 1`
		got := mysqlDialect{}.Rewrite(in)
		want := "INSERT INTO t (id,a,b) VALUES (?,?,?)\nON DUPLICATE KEY UPDATE a = VALUES(a), b = t.b + 1"
		if normSQL(got) != normSQL(want) {
			t.Errorf("got:  %q\nwant: %q", got, want)
		}
	})

	t.Run("DO NOTHING becomes INSERT IGNORE INTO and the clause disappears", func(t *testing.T) {
		in := `INSERT INTO t (event_id) VALUES (?) ON CONFLICT(event_id) DO NOTHING`
		got := mysqlDialect{}.Rewrite(in)
		want := `INSERT IGNORE INTO t (event_id) VALUES (?)`
		if normSQL(got) != normSQL(want) {
			t.Errorf("got:  %q\nwant: %q", got, want)
		}
	})

	t.Run("INSERT OR IGNORE INTO becomes INSERT IGNORE INTO", func(t *testing.T) {
		in := `INSERT OR IGNORE INTO events (id, kind) VALUES (?, ?)`
		got := mysqlDialect{}.Rewrite(in)
		want := `INSERT IGNORE INTO events (id, kind) VALUES (?, ?)`
		if normSQL(got) != normSQL(want) {
			t.Errorf("got:  %q\nwant: %q", got, want)
		}
	})

	t.Run("a conditional upsert (WHERE on the upsert branch) is left intact and marked unsupported", func(t *testing.T) {
		// This is the real canonical form of AcquireLoopLease's compare-and-set
		// (internal/store/loop_run_store.go's leaseSQL, non-MySQL branch): the
		// WHERE is what makes it atomic, and ON DUPLICATE KEY UPDATE has no WHERE.
		in := `
INSERT INTO loop_state (loop, lease_until, lease_owner)
VALUES (?, ?, ?)
ON CONFLICT(loop) DO UPDATE SET lease_until = excluded.lease_until, lease_owner = excluded.lease_owner
WHERE loop_state.lease_until IS NULL OR loop_state.lease_until < ?`
		got := mysqlDialect{}.Rewrite(in)
		if !strings.Contains(got, unsupportedUpsert) {
			t.Errorf("expected the unsupportedUpsert marker in output, got %q", got)
		}
		// The ON CONFLICT clause itself must survive unrewritten (no ON
		// DUPLICATE KEY UPDATE appears) since there is no MySQL form for it.
		if strings.Contains(strings.ToUpper(got), "ON DUPLICATE KEY UPDATE") {
			t.Errorf("a conditional upsert must not be rewritten into ON DUPLICATE KEY UPDATE, got %q", got)
		}
	})
}

// ---------------------------------------------------------------------------
// E. Postgres INSERT OR IGNORE -> ON CONFLICT DO NOTHING.

func TestPgInsertIgnore(t *testing.T) {
	t.Run("INSERT OR IGNORE becomes INSERT ... ON CONFLICT DO NOTHING", func(t *testing.T) {
		in := `INSERT OR IGNORE INTO events (id, kind) VALUES (?, ?)`
		got := pgInsertIgnore(in)
		want := `INSERT INTO events (id, kind) VALUES (?, ?) ON CONFLICT DO NOTHING`
		if normSQL(got) != normSQL(want) {
			t.Errorf("got:  %q\nwant: %q", got, want)
		}
	})

	t.Run("a statement that already has ON CONFLICT does not get a second clause", func(t *testing.T) {
		in := `INSERT OR IGNORE INTO t (a) VALUES (?) ON CONFLICT(a) DO NOTHING`
		got := pgInsertIgnore(in)
		if n := strings.Count(strings.ToUpper(got), "ON CONFLICT"); n != 1 {
			t.Errorf("expected exactly one ON CONFLICT clause, got %d in %q", n, got)
		}
	})

	t.Run("a statement with no INSERT OR IGNORE is untouched", func(t *testing.T) {
		in := `INSERT INTO t (a) VALUES (?) ON CONFLICT(a) DO UPDATE SET a = excluded.a`
		if got := pgInsertIgnore(in); got != in {
			t.Errorf("got:  %q\nwant: %q (unchanged)", got, in)
		}
	})
}

// ---------------------------------------------------------------------------
// F. MySQL reserved-word quoting.

func TestQuoteIdentsMySQLReserved(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "key, loop and cursor as identifiers get backticked",
			in:   `SELECT key, loop, cursor FROM t`,
			want: "SELECT `key`, `loop`, `cursor` FROM t",
		},
		{
			name: "PRIMARY KEY keeps an unquoted KEY",
			in:   `id TEXT PRIMARY KEY, name TEXT`,
			want: `id TEXT PRIMARY KEY, name TEXT`,
		},
		{
			name: "FOREIGN KEY keeps an unquoted KEY, but a real loop column is quoted",
			in:   `FOREIGN KEY (loop) REFERENCES x(loop)`,
			want: "FOREIGN KEY (`loop`) REFERENCES x(`loop`)",
		},
		{
			name: "ON DUPLICATE KEY UPDATE keeps an unquoted KEY",
			in:   `ON DUPLICATE KEY UPDATE key = VALUES(key)`,
			want: "ON DUPLICATE KEY UPDATE `key` = VALUES(`key`)",
		},
		{
			name: "loop_runs / loop_state are table names, not the reserved word loop",
			in:   `SELECT loop_runs.loop FROM loop_runs JOIN loop_state ON loop_runs.loop = loop_state.loop`,
			want: "SELECT loop_runs.`loop` FROM loop_runs JOIN loop_state ON loop_runs.`loop` = loop_state.`loop`",
		},
		{
			name: "excluded.key becomes excluded.`key`",
			in:   `SET seq = excluded.key`,
			want: "SET seq = excluded.`key`",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := quoteIdents(tc.in, mysqlReserved, '`', '`')
			if got != tc.want {
				t.Errorf("quoteIdents(%q)\n got:  %q\n want: %q", tc.in, got, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// G. DDL translation.

// The whole schema round-trips through the SQLite dialect unchanged. This is
// the test that protects every existing install: if it fails on anything
// that is not pure whitespace, the fix belongs in ddl.go, not here.
func TestSQLiteDDLIsByteIdenticalToCanonicalSchema(t *testing.T) {
	sch := buildSchema(migrations)
	for i, stmt := range migrations {
		out := sqliteDialect{}.DDL(stmt, sch)
		if normSQL(out) != normSQL(stmt) {
			t.Errorf("migrations[%d]: sqlite DDL output differs from canonical input beyond whitespace\nIN:  %s\nOUT: %s", i, stmt, out)
		}
	}
}

func TestPostgresDDLTypeTranslation(t *testing.T) {
	sch := buildSchema(migrations)

	t.Run("DATETIME becomes TIMESTAMP, datetime('now') default becomes now() AT TIME ZONE 'utc', autoincrement becomes BIGSERIAL", func(t *testing.T) {
		stmt := findMigration(t, "CREATE TABLE IF NOT EXISTS event_log")
		out := translateDDL(stmt, postgresTypes, sch)
		for _, want := range []string{
			"seq BIGSERIAL PRIMARY KEY",
			"created_at TIMESTAMP NOT NULL DEFAULT (now() AT TIME ZONE 'utc')",
		} {
			if !strings.Contains(normSQL(out), want) {
				t.Errorf("expected postgres DDL to contain %q, got:\n%s", want, out)
			}
		}
		if strings.Contains(strings.ToUpper(out), "DATETIME") {
			t.Errorf("postgres DDL must not contain the word DATETIME, got:\n%s", out)
		}
		if strings.Contains(strings.ToUpper(out), "AUTOINCREMENT") {
			t.Errorf("postgres DDL must not contain AUTOINCREMENT, got:\n%s", out)
		}
	})
}

func TestMySQLDDLTypeTranslation(t *testing.T) {
	sch := buildSchema(migrations)

	t.Run("kv_memory: composite-PK columns become VARCHAR(191), the value column becomes LONGTEXT", func(t *testing.T) {
		stmt := findMigration(t, "CREATE TABLE IF NOT EXISTS kv_memory")
		out := mysqlDialect{}.DDL(stmt, sch)
		for _, want := range []string{
			"`key` VARCHAR(191) NOT NULL",
			"grp VARCHAR(191) NOT NULL",
			"value LONGTEXT NOT NULL",
			"PRIMARY KEY (grp, `key`)",
		} {
			if !strings.Contains(normSQL(out), want) {
				t.Errorf("expected mysql DDL to contain %q, got:\n%s", want, out)
			}
		}
	})

	t.Run("memory_entries: content is LONGTEXT, the indexed namespace is VARCHAR(191)", func(t *testing.T) {
		stmt := findMigration(t, "CREATE TABLE IF NOT EXISTS memory_entries")
		out := mysqlDialect{}.DDL(stmt, sch)
		for _, want := range []string{
			"content LONGTEXT NOT NULL",
			"namespace VARCHAR(191) NOT NULL",
		} {
			if !strings.Contains(normSQL(out), want) {
				t.Errorf("expected mysql DDL to contain %q, got:\n%s", want, out)
			}
		}
	})

	t.Run("gitloom_path is added by ALTER TABLE and indexed later, and still gets the keyed VARCHAR treatment", func(t *testing.T) {
		stmt := findMigration(t, "ALTER TABLE memory_entries ADD COLUMN gitloom_path")
		out := mysqlDialect{}.DDL(stmt, sch)
		want := "ALTER TABLE memory_entries ADD COLUMN gitloom_path VARCHAR(191) NOT NULL DEFAULT ''"
		if normSQL(out) != normSQL(want) {
			t.Errorf("got:  %q\nwant: %q", out, want)
		}
	})

	t.Run("CREATE INDEX IF NOT EXISTS loses IF NOT EXISTS", func(t *testing.T) {
		stmt := findMigration(t, "CREATE INDEX IF NOT EXISTS idx_events_agent")
		out := mysqlDialect{}.DDL(stmt, sch)
		if strings.Contains(strings.ToUpper(out), "IF NOT EXISTS") {
			t.Errorf("mysql CREATE INDEX must drop IF NOT EXISTS, got %q", out)
		}
		want := "CREATE INDEX idx_events_agent ON events(agent_id)"
		if normSQL(out) != normSQL(want) {
			t.Errorf("got:  %q\nwant: %q", out, want)
		}
	})

	t.Run("tables gain the InnoDB/utf8mb4 suffix", func(t *testing.T) {
		stmt := findMigration(t, "CREATE TABLE IF NOT EXISTS event_log")
		out := mysqlDialect{}.DDL(stmt, sch)
		if !strings.HasSuffix(strings.TrimSpace(out), "ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci") {
			t.Errorf("expected the InnoDB/utf8mb4 suffix, got:\n%s", out)
		}
	})
}

// buildSchema correctness: every way a column can become "keyed".
func TestBuildSchemaFindsEveryKeyedColumn(t *testing.T) {
	sch := buildSchema(migrations)

	tests := []struct {
		table, column string
		want          bool
		reason        string
	}{
		{"kv_memory", "grp", true, "composite PRIMARY KEY (grp, key)"},
		{"kv_memory", "key", true, "composite PRIMARY KEY (grp, key)"},
		{"kv_memory", "value", false, "not part of any key or index"},
		{"agent_snapshots", "id", true, "inline TEXT PRIMARY KEY"},
		{"event_log", "event_id", true, "inline TEXT NOT NULL UNIQUE"},
		{"memory_entries", "namespace", true, "CREATE INDEX idx_mem_ns ON memory_entries(namespace)"},
		{"memory_entries", "content", false, "not part of any key or index"},
		{"reviews", "namespace", true, "CREATE UNIQUE INDEX idx_reviews_dedup ON reviews(namespace, dedup_key)"},
		{"reviews", "dedup_key", true, "CREATE UNIQUE INDEX idx_reviews_dedup ON reviews(namespace, dedup_key)"},
		{"memory_entries", "gitloom_path", true, "CREATE INDEX idx_mem_gitloom_path, added after the ALTER TABLE"},
	}
	for _, tc := range tests {
		if got := sch.isKeyed(tc.table, tc.column); got != tc.want {
			t.Errorf("isKeyed(%s.%s) = %v, want %v (%s)", tc.table, tc.column, got, tc.want, tc.reason)
		}
	}
}

// H. Guard test over the whole real schema, for MySQL: no unquoted reserved
// identifier survives, and no key/unique column is left as an unbounded
// LONGTEXT (which MySQL rejects with "BLOB/TEXT column used in key
// specification without a key length").
func TestMySQLSchemaGuardReservedWordsAndKeyedLongtext(t *testing.T) {
	sch := buildSchema(migrations)

	for _, stmt := range migrations {
		out := mysqlDialect{}.DDL(stmt, sch)

		// Comments and string/identifier literals are not SQL to quote or
		// type-check; work over the code regions only, the same regions the
		// production rewriters themselves are restricted to.
		code := codeRegionsOnly(out)

		words(code, func(w, prev string) string {
			lw := strings.ToLower(w)
			if lw != "key" && lw != "loop" && lw != "cursor" {
				return w
			}
			switch strings.ToUpper(prev) {
			case "PRIMARY", "FOREIGN", "UNIQUE", "DUPLICATE":
				return w // the KEY keyword, not an identifier
			}
			t.Errorf("unquoted reserved identifier %q (after %q) survived mysql DDL translation of: %s",
				w, prev, firstLine(stmt))
			return w
		})

		if idx := strings.Index(strings.ToUpper(code), "LONGTEXT"); idx >= 0 {
			// A LONGTEXT column is only a problem if it is also declared
			// PRIMARY KEY or UNIQUE on the same column definition.
			tail := strings.ToUpper(code[idx:])
			if end := strings.IndexAny(tail, ",)"); end >= 0 {
				tail = tail[:end]
			}
			if strings.Contains(tail, "PRIMARY KEY") || strings.Contains(tail, "UNIQUE") {
				t.Errorf("a LONGTEXT column is directly keyed (PRIMARY KEY/UNIQUE) in mysql DDL of: %s\n%s",
					firstLine(stmt), out)
			}
		}
	}
}

// codeRegionsOnly strips string/identifier literals and comments the same
// way scan() does, leaving only the text a rewriter is allowed to touch.
func codeRegionsOnly(q string) string {
	var b strings.Builder
	for _, p := range scan(q) {
		if p.code {
			b.WriteString(p.text)
		} else {
			b.WriteByte(' ')
		}
	}
	return b.String()
}

// buildSchema uses a naive stripComment (truncate the whole remaining item
// at the first "--") to find PRIMARY KEY / UNIQUE columns, rather than the
// codeMask-based, comment-boundary-aware scan that translateColumnDef now
// uses (see the comment on translateColumnDef in ddl.go). Because a trailing
// `--` comment on one column's line lands at the head of the NEXT column's
// split item, a column whose own PRIMARY KEY/UNIQUE modifier immediately
// follows an annotated column is invisible to buildSchema: the item's
// content after the comment is discarded before the PRIMARY KEY/UNIQUE check
// ever runs.
//
// No table in the current migrations.go happens to put a keyed column right
// after a commented one, so this does not corrupt today's schema. But the
// failure mode is real and silent: on MySQL, translateColumnDef would then
// map that column's TEXT to LONGTEXT (unkeyed) instead of VARCHAR(191), and
// MySQL rejects a LONGTEXT column used as a key ("BLOB/TEXT column used in
// key specification without a key length"). This test pins the bug with a
// minimal repro; it is expected to FAIL until buildSchema's stripComment is
// replaced with the same comment-boundary-aware scan translateColumnDef uses.
// A trailing comment on one column is attached to the head of the next one by
// splitTop, and truncating there once hid whatever that column declared. On
// MySQL the missed column stayed LONGTEXT and its key was rejected outright.
func TestBuildSchemaSeesPastATrailingComment(t *testing.T) {
	synthetic := []string{
		`CREATE TABLE IF NOT EXISTS widget (
			a TEXT NOT NULL,          -- a trailing comment on the previous column
			b TEXT UNIQUE,
			c TEXT NOT NULL,          -- and one before a composite key
			PRIMARY KEY (a, c)
		)`,
	}
	sch := buildSchema(synthetic)
	for _, col := range []string{"a", "b", "c"} {
		if !sch.isKeyed("widget", col) {
			t.Errorf("widget.%s not marked keyed", col)
		}
	}

	got := translateColumnDef("\n\t-- trailing\n\tb TEXT UNIQUE", mysqlTypes, sch, "widget")
	if !strings.Contains(got, "VARCHAR(191)") {
		t.Errorf("keyed column stayed unbounded text: %q", got)
	}
}

// ---------------------------------------------------------------------------
// I. ParseDSN.

func TestParseDSN(t *testing.T) {
	t.Run("a bare filesystem path is sqlite with the required pragmas appended", func(t *testing.T) {
		d, err := ParseDSN("/home/me/.karmax/db/karmax.db")
		if err != nil {
			t.Fatalf("ParseDSN: %v", err)
		}
		if d.Kind != SQLite || d.Driver != "sqlite3" {
			t.Fatalf("got Kind=%v Driver=%v", d.Kind, d.Driver)
		}
		want := "/home/me/.karmax/db/karmax.db?_journal_mode=WAL&_busy_timeout=5000&_foreign_keys=on"
		if d.Conn != want {
			t.Errorf("Conn = %q, want %q", d.Conn, want)
		}
	})

	t.Run("a path that already has a query string is not double-appended", func(t *testing.T) {
		in := "/home/me/db.sqlite?_journal_mode=WAL"
		d, err := ParseDSN(in)
		if err != nil {
			t.Fatalf("ParseDSN: %v", err)
		}
		if d.Conn != in {
			t.Errorf("Conn = %q, want unchanged %q", d.Conn, in)
		}
	})

	for _, prefix := range []string{"postgres://", "postgresql://"} {
		t.Run(prefix+" is postgres, driver pgx, TimeZone=UTC present", func(t *testing.T) {
			raw := prefix + "user:pass@host:5432/karmax?sslmode=require"
			d, err := ParseDSN(raw)
			if err != nil {
				t.Fatalf("ParseDSN: %v", err)
			}
			if d.Kind != Postgres || d.Driver != "pgx" {
				t.Fatalf("got Kind=%v Driver=%v", d.Kind, d.Driver)
			}
			if !strings.Contains(d.Conn, "TimeZone=UTC") {
				t.Errorf("Conn = %q, missing TimeZone=UTC", d.Conn)
			}
			if !strings.Contains(d.Conn, "sslmode=require") {
				t.Errorf("Conn = %q, lost the caller's sslmode=require", d.Conn)
			}
		})
	}

	t.Run("mysql:// is mysql with parseTime=true and a UTC loc in the formatted DSN", func(t *testing.T) {
		d, err := ParseDSN("mysql://user:pass@host:3306/karmax")
		if err != nil {
			t.Fatalf("ParseDSN: %v", err)
		}
		if d.Kind != MySQL || d.Driver != "mysql" {
			t.Fatalf("got Kind=%v Driver=%v", d.Kind, d.Driver)
		}
		if !strings.Contains(d.Conn, "parseTime=true") {
			t.Errorf("Conn = %q, missing parseTime=true", d.Conn)
		}
		cfg, err := mysql.ParseDSN(d.Conn)
		if err != nil {
			t.Fatalf("the produced DSN does not parse: %v", err)
		}
		if cfg.Loc != time.UTC {
			t.Errorf("Loc = %v, want time.UTC", cfg.Loc)
		}
	})

	t.Run("a go-sql-driver native DSN is recognised as mysql", func(t *testing.T) {
		d, err := ParseDSN("user:pass@tcp(host:3306)/karmax")
		if err != nil {
			t.Fatalf("ParseDSN: %v", err)
		}
		if d.Kind != MySQL || d.Driver != "mysql" {
			t.Fatalf("got Kind=%v Driver=%v", d.Kind, d.Driver)
		}
		cfg, err := mysql.ParseDSN(d.Conn)
		if err != nil {
			t.Fatalf("the produced DSN does not parse: %v", err)
		}
		if !cfg.ParseTime {
			t.Errorf("ParseTime = false, want true")
		}
		if cfg.Loc != time.UTC {
			t.Errorf("Loc = %v, want time.UTC", cfg.Loc)
		}
	})

	t.Run("empty DSN is an error", func(t *testing.T) {
		if _, err := ParseDSN("   "); err == nil {
			t.Error("expected an error for an empty DSN")
		}
	})
}

// ---------------------------------------------------------------------------
// J. utcArgs.

func TestUTCArgs(t *testing.T) {
	ist, err := time.LoadLocation("Asia/Kolkata")
	if err != nil {
		t.Skipf("tzdata unavailable: %v", err)
	}

	t.Run("a time.Time in a non-UTC location is converted to UTC, same instant", func(t *testing.T) {
		local := time.Date(2026, 8, 22, 18, 30, 0, 0, ist)
		out := utcArgs([]any{local})
		got, ok := out[0].(time.Time)
		if !ok {
			t.Fatalf("expected time.Time, got %T", out[0])
		}
		if got.Location() != time.UTC {
			t.Errorf("Location() = %v, want UTC", got.Location())
		}
		if !got.Equal(local) {
			t.Errorf("got %v does not represent the same instant as %v", got, local)
		}
	})

	t.Run("a *time.Time is converted the same way", func(t *testing.T) {
		local := time.Date(2026, 8, 22, 18, 30, 0, 0, ist)
		out := utcArgs([]any{&local})
		got, ok := out[0].(time.Time)
		if !ok {
			t.Fatalf("expected out[0] to become a time.Time value, got %T", out[0])
		}
		if got.Location() != time.UTC || !got.Equal(local) {
			t.Errorf("got %v, want the same instant in UTC", got)
		}
	})

	t.Run("a nil *time.Time does not panic and passes through", func(t *testing.T) {
		var nilTime *time.Time
		out := utcArgs([]any{nilTime})
		got, ok := out[0].(*time.Time)
		if !ok || got != nil {
			t.Errorf("got %#v, want a nil *time.Time", out[0])
		}
	})

	t.Run("non-time arguments pass through untouched", func(t *testing.T) {
		in := []any{"hello", 42, 3.14, true, nil}
		out := utcArgs(in)
		for i := range in {
			if out[i] != in[i] {
				t.Errorf("arg[%d] = %#v, want unchanged %#v", i, out[i], in[i])
			}
		}
	})
}
