package store

import (
	"regexp"
	"sort"
	"strings"
)

// Schema translation.
//
// migrations.go is written in SQLite DDL and stays that way: it is the
// canonical schema, and the SQLite output of this file is byte-identical to
// what it already says, so no existing database is touched by any of this.
//
// The one fact SQLite's DDL does not record is which TEXT columns are short
// enough to be a key. MySQL needs it — a LONGTEXT cannot be indexed without a
// prefix length — so schemaInfo recovers it from the schema itself rather than
// from an annotation somebody has to remember to add.

type schemaInfo struct {
	// keyed[table][column] is true where the column is part of a primary key,
	// a unique constraint, or any index.
	keyed map[string]map[string]bool
}

var (
	reCreateTable = regexp.MustCompile(`(?is)^\s*CREATE\s+TABLE\s+(?:IF\s+NOT\s+EXISTS\s+)?([\w.]+)\s*\((.*)\)\s*$`)
	reCreateIndex = regexp.MustCompile(`(?is)^\s*CREATE\s+(UNIQUE\s+)?INDEX\s+(?:IF\s+NOT\s+EXISTS\s+)?(\w+)\s+ON\s+(\w+)\s*\((.*)\)\s*$`)
	reAlterAdd    = regexp.MustCompile(`(?is)^\s*ALTER\s+TABLE\s+(\w+)\s+ADD\s+COLUMN\s+(\w+)\s+(.*)$`)
	reAutoInc     = regexp.MustCompile(`(?is)\bINTEGER\s+PRIMARY\s+KEY\s+AUTOINCREMENT\b`)
	reNowDefault  = regexp.MustCompile(`(?is)DEFAULT\s*\(\s*datetime\('now'\)\s*\)`)
)

// buildSchema reads the whole migration list and records every column that
// something keys on.
func buildSchema(stmts []string) *schemaInfo {
	s := &schemaInfo{keyed: map[string]map[string]bool{}}
	for _, stmt := range stmts {
		if m := reCreateIndex.FindStringSubmatch(stmt); m != nil {
			for _, col := range splitTop(m[4]) {
				s.mark(m[3], indexedColumn(col))
			}
			continue
		}
		m := reCreateTable.FindStringSubmatch(stmt)
		if m == nil {
			continue
		}
		table := m[1]
		for _, item := range splitTop(m[2]) {
			// Read through a mask rather than the raw text: a trailing `--`
			// comment on the previous column arrives at the head of this item,
			// and truncating there hid whatever the item actually declared.
			mask := codeMask(item)
			head, _, headEnd, ok := nextIdent(mask, 0)
			if !ok {
				continue
			}
			switch head {
			case "PRIMARY", "UNIQUE":
				open := strings.IndexByte(mask, '(')
				if open < 0 {
					continue
				}
				end := matchParens(mask, open)
				for _, col := range splitTop(item[open+1 : end-1]) {
					s.mark(table, indexedColumn(col))
				}
			case "FOREIGN", "CHECK", "CONSTRAINT":
				// not a column definition
			default:
				rest := mask[headEnd:]
				if strings.Contains(rest, "PRIMARY KEY") || strings.Contains(rest, "UNIQUE") {
					s.mark(table, head)
				}
			}
		}
	}
	return s
}

func (s *schemaInfo) mark(table, col string) {
	table, col = strings.ToLower(table), strings.ToLower(col)
	if col == "" {
		return
	}
	if s.keyed[table] == nil {
		s.keyed[table] = map[string]bool{}
	}
	s.keyed[table][col] = true
}

func (s *schemaInfo) isKeyed(table, col string) bool {
	if s == nil {
		return false
	}
	return s.keyed[strings.ToLower(table)][strings.ToLower(col)]
}

// indexedColumn strips the sort direction from an index column spec.
func indexedColumn(spec string) string {
	f := strings.Fields(strings.TrimSpace(spec))
	if len(f) == 0 {
		return ""
	}
	return f[0]
}

func firstWord(s string) string {
	f := strings.Fields(strings.TrimSpace(s))
	if len(f) == 0 {
		return ""
	}
	return f[0]
}

// splitTop splits on commas that are not inside parentheses, a string literal
// or a comment.
func splitTop(s string) []string {
	var out []string
	depth, start := 0, 0
	inStr := false
	inLine := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case inLine:
			if c == '\n' {
				inLine = false
			}
		case inStr:
			if c == '\'' {
				if i+1 < len(s) && s[i+1] == '\'' {
					i++
					continue
				}
				inStr = false
			}
		case c == '\'':
			inStr = true
		case c == '-' && i+1 < len(s) && s[i+1] == '-':
			inLine = true
		case c == '(':
			depth++
		case c == ')':
			depth--
		case c == ',' && depth == 0:
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	out = append(out, s[start:])
	return out
}

// typeMap is one backend's spelling of the canonical SQLite types.
type typeMap struct {
	text     string // unbounded text
	keyText  string // text short enough to index
	integer  string
	bigint   string
	datetime string
	real     string
	blob     string
	// autoInc replaces "INTEGER PRIMARY KEY AUTOINCREMENT" wholesale.
	autoInc string
	// nowDefault replaces "DEFAULT (datetime('now'))".
	nowDefault string
	// tableSuffix is appended to CREATE TABLE (MySQL storage engine, charset).
	tableSuffix string
	// indexIfNotExists is false where the backend rejects the clause.
	indexIfNotExists bool
}

// isSchemaStmt reports whether a migration entry is DDL. The list also holds a
// data-moving INSERT, which belongs to the DML rewriter instead.
func isSchemaStmt(stmt string) bool {
	switch strings.ToUpper(firstWord(strings.TrimSpace(stmt))) {
	case "CREATE", "ALTER", "DROP":
		return true
	}
	return false
}

// translateDDL rewrites one canonical schema statement for a backend.
func translateDDL(stmt string, tm typeMap, sch *schemaInfo) string {
	switch {
	case reCreateTable.MatchString(stmt):
		return translateCreateTable(stmt, tm, sch)
	case reCreateIndex.MatchString(stmt):
		return translateCreateIndex(stmt, tm)
	case reAlterAdd.MatchString(stmt):
		m := reAlterAdd.FindStringSubmatch(stmt)
		def := translateColumnDef(m[2]+" "+m[3], tm, sch, m[1])
		return "ALTER TABLE " + m[1] + " ADD COLUMN " + def
	default:
		// DROP TABLE, data-moving INSERTs and anything else portable enough to
		// pass through the DML rewriter.
		return stmt
	}
}

func translateCreateTable(stmt string, tm typeMap, sch *schemaInfo) string {
	m := reCreateTable.FindStringSubmatch(stmt)
	table := m[1]

	items := splitTop(m[2])
	out := make([]string, 0, len(items))
	for _, item := range items {
		out = append(out, translateColumnDef(item, tm, sch, table))
	}

	return "CREATE TABLE IF NOT EXISTS " + table + " (" + strings.Join(out, ",") + ")" + tm.tableSuffix
}

// translateColumnDef rewrites the type and the now-default of one column
// definition IN PLACE.
//
// In place, rather than by reassembling name + type + modifiers, because a
// definition is not only a definition: migrations.go annotates columns with
// trailing `--` comments, and a comment belonging to the previous column
// arrives at the head of the next one. Rebuilding threw those away and — worse
// — made a commented column look like an empty item, so the column after every
// annotated one was passed through untranslated. Editing spans keeps the
// original text exactly, comments and column alignment included.
func translateColumnDef(def string, tm typeMap, sch *schemaInfo, table string) string {
	mask := codeMask(def)

	name, _, nameEnd, ok := nextIdent(mask, 0)
	if !ok {
		return def
	}
	switch name {
	case "PRIMARY", "UNIQUE", "FOREIGN", "CHECK", "CONSTRAINT":
		return def
	}

	var edits []edit

	if loc := reNowDefault.FindStringIndex(mask); loc != nil {
		edits = append(edits, edit{loc[0], loc[1], tm.nowDefault})
	}

	if loc := reAutoInc.FindStringIndex(mask); loc != nil && tm.autoInc != "" {
		edits = append(edits, edit{loc[0], loc[1], tm.autoInc})
	} else if typ, ts, te, ok := nextIdent(mask, nameEnd); ok {
		edits = append(edits, edit{ts, te, mapType(typ, tm, sch, table, name)})
	}

	return applyEdits(def, edits)
}

func mapType(typ string, tm typeMap, sch *schemaInfo, table, column string) string {
	switch typ {
	case "TEXT":
		if sch.isKeyed(table, column) {
			return tm.keyText
		}
		return tm.text
	case "INTEGER", "INT":
		return tm.integer
	case "BIGINT":
		return tm.bigint
	case "DATETIME", "TIMESTAMP":
		return tm.datetime
	case "REAL", "DOUBLE":
		return tm.real
	case "BLOB":
		return tm.blob
	}
	return typ
}

type edit struct {
	start, end int
	text       string
}

// applyEdits rewrites spans back to front so earlier offsets stay valid.
func applyEdits(s string, edits []edit) string {
	sort.Slice(edits, func(i, j int) bool { return edits[i].start > edits[j].start })
	for _, e := range edits {
		s = s[:e.start] + e.text + s[e.end:]
	}
	return s
}

// nextIdent returns the identifier at or after i in a masked statement, with
// its span. The mask is upper-cased, so the word comes back upper-cased too.
func nextIdent(mask string, i int) (word string, start, end int, ok bool) {
	for i < len(mask) && !isIdentByte(mask[i]) {
		i++
	}
	if i >= len(mask) {
		return "", 0, 0, false
	}
	j := i
	for j < len(mask) && isIdentByte(mask[j]) {
		j++
	}
	return mask[i:j], i, j, true
}

func translateCreateIndex(stmt string, tm typeMap) string {
	m := reCreateIndex.FindStringSubmatch(stmt)
	unique := strings.TrimSpace(m[1])
	if unique != "" {
		unique = "UNIQUE "
	}
	guard := "IF NOT EXISTS "
	if !tm.indexIfNotExists {
		guard = ""
	}
	return "CREATE " + unique + "INDEX " + guard + m[2] + " ON " + m[3] + "(" + m[4] + ")"
}
