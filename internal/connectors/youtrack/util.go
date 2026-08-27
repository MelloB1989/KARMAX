package youtrack

import (
	"net/url"
	"time"
)

// Argument coercion. A model sends what it sends: a number as a string, a
// string where an int belongs. Coercing beats rejecting, because the failure
// mode of rejecting is the model retrying the same call with the same shape.

func strArg(in map[string]any, key string) string {
	v, ok := in[key]
	if !ok || v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

func intArg(in map[string]any, key string, def int) int {
	v, ok := in[key]
	if !ok || v == nil {
		return def
	}
	switch n := v.(type) {
	case float64: // every JSON number decodes to float64
		if n <= 0 {
			return def
		}
		return int(n)
	case int:
		if n <= 0 {
			return def
		}
		return n
	}
	return def
}

// millisToRFC3339 renders a YouTrack timestamp.
//
// YouTrack sends epoch MILLISECONDS. Passing that straight to time.Unix reads
// it as seconds and lands in the year 58000, which looks like a parsing bug
// downstream rather than a units one here.
func millisToRFC3339(ms int64) string {
	if ms <= 0 {
		return ""
	}
	return time.UnixMilli(ms).UTC().Format(time.RFC3339)
}

// truncate shortens to n runes, never splitting a character.
func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// urlQuery escapes a YouTrack query string for the query component.
func urlQuery(s string) string { return url.QueryEscape(s) }

// urlPath escapes an issue id for use in a path segment.
func urlPath(s string) string { return url.PathEscape(s) }
