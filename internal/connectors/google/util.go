package google

import (
	"fmt"
	"regexp"
	"strings"
	"time"
)

func strArg(in map[string]any, key string) string {
	if v, ok := in[key].(string); ok {
		return v
	}
	return ""
}

func intArg(in map[string]any, key string, def int) int {
	switch n := in[key].(type) {
	case float64:
		if n > 0 {
			return int(n)
		}
	case int:
		if n > 0 {
			return n
		}
	}
	return def
}

func clamp(n, lo, hi int) int {
	if n < lo {
		return lo
	}
	if n > hi {
		return hi
	}
	return n
}

// timeArg accepts either a full timestamp or a bare date.
//
// A model asked for "tomorrow" will send 2026-08-28 as often as it sends a full
// RFC3339 stamp, and rejecting the short form just makes it guess again.
func timeArg(in map[string]any, key string, def time.Time) (time.Time, error) {
	raw := strings.TrimSpace(strArg(in, key))
	if raw == "" {
		return def, nil
	}
	if t, err := time.Parse(time.RFC3339, raw); err == nil {
		return t, nil
	}
	if t, err := time.Parse("2006-01-02", raw); err == nil {
		return t, nil
	}
	return time.Time{}, fmt.Errorf("google: %q must be RFC3339 or YYYY-MM-DD", key)
}

// truncate shortens to n runes, never splitting a character.
func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

var (
	htmlTag    = regexp.MustCompile(`(?s)<[^>]*>`)
	htmlScript = regexp.MustCompile(`(?is)<(script|style)[^>]*>.*?</(script|style)>`)
	blankRuns  = regexp.MustCompile(`\n{3,}`)
	htmlEntity = strings.NewReplacer("&nbsp;", " ", "&amp;", "&", "&lt;", "<", "&gt;", ">", "&quot;", `"`, "&#39;", "'")
)

// stripHTML renders an HTML mail body as rough text.
//
// Only used when a message has no text/plain part at all. Handing raw HTML to a
// model spends most of the context window on markup and invites it to quote
// tags back at a human.
func stripHTML(s string) string {
	if s == "" {
		return ""
	}
	s = htmlScript.ReplaceAllString(s, "")
	s = htmlTag.ReplaceAllString(s, "")
	s = htmlEntity.Replace(s)
	return strings.TrimSpace(blankRuns.ReplaceAllString(s, "\n\n"))
}
