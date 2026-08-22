package store

import (
	"strconv"
	"strings"
	"unicode"
)

// The SQL rewriter.
//
// Every rewrite here is a text transform, which is only safe because it never
// looks inside a string literal, a quoted identifier, or a comment. scan()
// splits a statement into those regions once; the rules below then run over the
// code regions alone. Without that split, rebinding `?` would renumber a
// question mark inside a LIKE pattern and quoting an identifier would rewrite
// the operator's own text.

type piece struct {
	text string
	// code is false for string literals, quoted identifiers and comments —
	// regions no rule may touch.
	code bool
}

// scan splits SQL into code and untouchable regions.
//
// `datetime('now')` is emitted as a single code piece even though it contains a
// literal, because it is one token as far as the rewriter is concerned.
func scan(q string) []piece {
	var out []piece
	var buf strings.Builder

	flush := func() {
		if buf.Len() > 0 {
			out = append(out, piece{text: buf.String(), code: true})
			buf.Reset()
		}
	}

	for i := 0; i < len(q); {
		// datetime('now') — atomic, so the rule for it sees one token.
		if n := matchNow(q, i); n > 0 {
			flush()
			out = append(out, piece{text: q[i : i+n], code: true})
			i += n
			continue
		}

		c := q[i]
		switch {
		case c == '\'', c == '"', c == '`':
			flush()
			j := closingQuote(q, i)
			out = append(out, piece{text: q[i:j], code: false})
			i = j

		case c == '-' && i+1 < len(q) && q[i+1] == '-':
			flush()
			j := strings.IndexByte(q[i:], '\n')
			if j < 0 {
				out = append(out, piece{text: q[i:], code: false})
				i = len(q)
			} else {
				out = append(out, piece{text: q[i : i+j], code: false})
				i += j
			}

		case c == '/' && i+1 < len(q) && q[i+1] == '*':
			flush()
			j := strings.Index(q[i+2:], "*/")
			if j < 0 {
				out = append(out, piece{text: q[i:], code: false})
				i = len(q)
			} else {
				end := i + 2 + j + 2
				out = append(out, piece{text: q[i:end], code: false})
				i = end
			}

		default:
			buf.WriteByte(c)
			i++
		}
	}
	flush()
	return out
}

// closingQuote returns the index just past the literal starting at i.
// A doubled quote is an escaped quote, not a terminator.
func closingQuote(q string, i int) int {
	quote := q[i]
	for j := i + 1; j < len(q); j++ {
		if q[j] != quote {
			continue
		}
		if j+1 < len(q) && q[j+1] == quote {
			j++
			continue
		}
		return j + 1
	}
	return len(q)
}

// matchNow reports the length of a `datetime('now')` token at i, or 0.
func matchNow(q string, i int) int {
	const lit = "datetime('now')"
	if i+len(lit) > len(q) {
		return 0
	}
	if !strings.EqualFold(q[i:i+len(lit)], lit) {
		return 0
	}
	return len(lit)
}

// mapCode applies fn to every code region and reassembles the statement.
func mapCode(q string, fn func(string) string) string {
	var b strings.Builder
	for _, p := range scan(q) {
		if p.code {
			b.WriteString(fn(p.text))
		} else {
			b.WriteString(p.text)
		}
	}
	return b.String()
}

// replaceNow substitutes the backend's UTC-now expression.
func replaceNow(q, now string) string {
	var b strings.Builder
	for _, p := range scan(q) {
		if p.code && matchNow(p.text, 0) == len(p.text) {
			b.WriteString(now)
			continue
		}
		b.WriteString(p.text)
	}
	return b.String()
}

// rebind renumbers `?` placeholders into Postgres `$1..$n`.
func rebind(q string) string {
	n := 0
	var b strings.Builder
	for _, p := range scan(q) {
		if !p.code {
			b.WriteString(p.text)
			continue
		}
		for i := 0; i < len(p.text); i++ {
			if p.text[i] == '?' {
				n++
				b.WriteByte('$')
				b.WriteString(strconv.Itoa(n))
				continue
			}
			b.WriteByte(p.text[i])
		}
	}
	return b.String()
}

// isIdentByte reports whether c can appear inside an unquoted identifier.
func isIdentByte(c byte) bool {
	return c == '_' || c == '$' ||
		(c >= '0' && c <= '9') ||
		(c >= 'a' && c <= 'z') ||
		(c >= 'A' && c <= 'Z')
}

// words walks the identifiers in a code region, handing each to fn along with
// the identifier that preceded it. fn returns the replacement text.
func words(code string, fn func(word, prev string) string) string {
	var b strings.Builder
	prev := ""
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
		// A qualified name (`t.col`) is still one identifier per part, but the
		// dot tells us the part before it was a table, not a keyword.
		b.WriteString(fn(w, prev))
		prev = w
		i = j
	}
	return b.String()
}

// quoteIdents wraps reserved column names in the backend's identifier quotes.
//
// `key` is skipped after PRIMARY/FOREIGN/UNIQUE/DUPLICATE, where it is the
// keyword rather than kv_memory's column.
func quoteIdents(q string, reserved map[string]bool, open, close byte) string {
	return mapCode(q, func(code string) string {
		return words(code, func(w, prev string) string {
			if !reserved[strings.ToLower(w)] {
				return w
			}
			switch strings.ToUpper(prev) {
			case "PRIMARY", "FOREIGN", "UNIQUE", "DUPLICATE":
				return w
			}
			return string(open) + w + string(close)
		})
	})
}

// callArgs returns the top-level arguments of the call whose opening paren is
// at open, and the index just past its closing paren.
func callArgs(code string, open int) ([]string, int) {
	depth := 0
	start := open + 1
	var args []string
	for i := open; i < len(code); i++ {
		switch code[i] {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				args = append(args, code[start:i])
				return args, i + 1
			}
		case ',':
			if depth == 1 {
				args = append(args, code[start:i])
				start = i + 1
			}
		}
	}
	return nil, len(code)
}

// rewriteCall replaces every call to name whose argument count matches, using
// build to produce the replacement from the argument text.
func rewriteCall(q, name string, arity int, build func(args []string) string) string {
	return mapCode(q, func(code string) string {
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
			k := j
			for k < len(code) && unicode.IsSpace(rune(code[k])) {
				k++
			}
			if !strings.EqualFold(w, name) || k >= len(code) || code[k] != '(' {
				b.WriteString(w)
				i = j
				continue
			}
			args, end := callArgs(code, k)
			if len(args) != arity {
				b.WriteString(w)
				i = j
				continue
			}
			b.WriteString(build(args))
			i = end
		}
		return b.String()
	})
}

// renameTwoArg rewrites the scalar two-argument form of a function.
//
// SQLite overloads MAX/MIN: one argument is the aggregate, two are the scalar
// greater/lesser. Postgres and MySQL spell the scalar form GREATEST/LEAST and
// would otherwise reject the call outright.
func renameTwoArg(q, from, to string) string {
	return rewriteCall(q, from, 2, func(a []string) string {
		return to + "(" + a[0] + "," + a[1] + ")"
	})
}

// rewriteDayString rewrites SQLite's date(x).
//
// date() yields the TEXT "YYYY-MM-DD", and callers scan it into a string.
// Postgres would hand back a date value instead and fail the scan, so every
// backend is asked for the same formatted string rather than for a date.
func rewriteDayString(q string, format func(arg string) string) string {
	return rewriteCall(q, "date", 1, func(a []string) string {
		return format(strings.TrimSpace(a[0]))
	})
}

// indexFold finds sub in q's code regions, case-insensitively. It searches the
// mask so a verb rewrite can never fire on a word inside a string literal.
func indexFold(q, sub string) int {
	return strings.Index(codeMask(q), strings.ToUpper(sub))
}

// codeMask returns q with every literal, quoted identifier and comment blanked
// to spaces of the same length. Offsets found in the mask are valid in q, which
// is what lets the clause rewrites below search for keywords without ever
// matching one that appears inside the operator's own text.
func codeMask(q string) string {
	out := make([]byte, 0, len(q))
	for _, p := range scan(q) {
		if p.code {
			out = append(out, []byte(strings.ToUpper(p.text))...)
			continue
		}
		for i := 0; i < len(p.text); i++ {
			out = append(out, ' ')
		}
	}
	return string(out)
}

// findKeyword returns the offset of an upper-case keyword in the mask at
// identifier boundaries, or -1.
func findKeyword(mask, word string, from int) int {
	for i := from; ; {
		j := strings.Index(mask[i:], word)
		if j < 0 {
			return -1
		}
		at := i + j
		before := at == 0 || !isIdentByte(mask[at-1])
		afterAt := at + len(word)
		after := afterAt >= len(mask) || !isIdentByte(mask[afterAt])
		if before && after {
			return at
		}
		i = at + len(word)
	}
}

// skipSpace returns the first non-space offset at or after i.
func skipSpace(s string, i int) int {
	for i < len(s) && (s[i] == ' ' || s[i] == '\t' || s[i] == '\n' || s[i] == '\r') {
		i++
	}
	return i
}

// matchParens returns the offset just past the parenthesis group opening at i.
func matchParens(s string, i int) int {
	depth := 0
	for ; i < len(s); i++ {
		switch s[i] {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return i + 1
			}
		}
	}
	return len(s)
}

// millisBetween is the backend's expression for the milliseconds between a
// timestamp column and a bound parameter.
//
// SQLite spells it with julianday, which neither server database has. There is
// no portable form, so the caller asks for one rather than writing SQLite's.
func millisBetween(k Kind, start, end string) string {
	switch k {
	case Postgres:
		// The parameter is cast explicitly: pgx sends a time.Time as
		// timestamptz, and subtracting it from a timestamp column is ambiguous.
		return "CAST(EXTRACT(EPOCH FROM (CAST(" + end + " AS TIMESTAMP) - " + start + ")) * 1000 AS BIGINT)"
	case MySQL:
		return "TIMESTAMPDIFF(MICROSECOND, " + start + ", " + end + ") / 1000"
	default:
		return "CAST((julianday(" + end + ") - julianday(" + start + ")) * 86400000 AS INTEGER)"
	}
}
