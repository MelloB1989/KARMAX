package memory

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"
)

// Where a piece of work came from, carried to the tools that write and read
// memory so they can tag it without every caller threading it through.

type sourceKey struct{}

// Source is who and where the work being done came from.
type Source struct {
	Person string
	Chat   string
	// At is when the triggering message was sent, which is when what the agent
	// learns from it happened.
	At time.Time
}

// WithSource attaches the origin of the current work to ctx.
func WithSource(ctx context.Context, s Source) context.Context {
	return context.WithValue(ctx, sourceKey{}, s)
}

// SourceFrom returns the origin attached to ctx, zero when there is none.
func SourceFrom(ctx context.Context) Source {
	s, _ := ctx.Value(sourceKey{}).(Source)
	return s
}

type windowKey struct{}

// Window is a time range a question is bounded to.
type Window struct{ Since, Until time.Time }

// WithWindow bounds retrievals made under ctx to a time range.
func WithWindow(ctx context.Context, w Window) context.Context {
	if w.Since.IsZero() && w.Until.IsZero() {
		return ctx
	}
	return context.WithValue(ctx, windowKey{}, w)
}

// WindowFrom returns the range attached to ctx, zero when unbounded.
func WindowFrom(ctx context.Context) Window {
	w, _ := ctx.Value(windowKey{}).(Window)
	return w
}

// ParseWhen reads a time a caller wrote: RFC 3339, a datetime, or a bare date
// in loc. A bare Until date is the END of that day, so "until Tuesday" includes
// Tuesday.
func ParseWhen(s string, loc *time.Location, endOfDay bool) (time.Time, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, false
	}
	if loc == nil {
		loc = time.UTC
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, true
	}
	for _, layout := range []string{"2006-01-02T15:04:05", "2006-01-02 15:04"} {
		if t, err := time.ParseInLocation(layout, s, loc); err == nil {
			return t, true
		}
	}
	if t, err := time.ParseInLocation("2006-01-02", s, loc); err == nil {
		if endOfDay {
			t = t.AddDate(0, 0, 1).Add(-time.Second)
		}
		return t, true
	}
	return time.Time{}, false
}

// PersonTag and ChatTag are the tags a write carries so a later question can
// be scoped to one person or one conversation.
func PersonTag(id string) string {
	if strings.TrimSpace(id) == "" {
		return ""
	}
	return "person:" + sanitiseMember(id)
}

func ChatTag(id string) string {
	id = strings.ToLower(strings.TrimSpace(id))
	if i := strings.IndexAny(id, "@:"); i >= 0 {
		id = id[:i]
	}
	if id == "" {
		return ""
	}
	return "chat:" + sanitiseMember(id)
}

// The server's tag rules: letters, digits, spaces and - _ . : / # @, at most
// 32 tags of 64 characters, and one that breaks them refuses the whole write.
const (
	maxTags   = 32
	maxTagLen = 64
)

func cleanTag(t string) string {
	t = strings.ToLower(strings.TrimSpace(t))
	var b strings.Builder
	n := 0
	for _, r := range t {
		if n >= maxTagLen {
			break
		}
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r), strings.ContainsRune(" -_.:/#@", r):
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
		n++
	}
	return strings.TrimSpace(b.String())
}

// unionTags folds incoming onto existing, both cleaned to the server's rules.
// Past the cap it is the existing tags that give way: what this write is about
// outranks what an earlier one was.
func unionTags(existing, incoming []string) []string {
	seen := map[string]bool{}
	pick := func(in []string, room int) []string {
		var out []string
		for _, t := range in {
			t = cleanTag(t)
			if t == "" || seen[t] || len(out) >= room {
				continue
			}
			seen[t] = true
			out = append(out, t)
		}
		return out
	}
	inc := pick(incoming, maxTags)
	old := pick(existing, maxTags-len(inc))
	return append(old, inc...)
}

// SystemTimezone is the operator's zone name: KARMAX_TIMEZONE, then TZ, then the
// host's. Empty when none can be read, which the server takes as UTC.
func SystemTimezone() string {
	for _, k := range []string{"KARMAX_TIMEZONE", "TZ"} {
		if v := strings.TrimSpace(os.Getenv(k)); v != "" {
			if _, err := time.LoadLocation(v); err == nil {
				return v
			}
		}
	}
	if n := time.Local.String(); n != "" && n != "Local" {
		return n
	}
	if b, err := os.ReadFile("/etc/timezone"); err == nil {
		if v := strings.TrimSpace(string(b)); v != "" {
			return v
		}
	}
	if p, err := filepath.EvalSymlinks("/etc/localtime"); err == nil {
		if i := strings.Index(p, "zoneinfo/"); i >= 0 {
			return p[i+len("zoneinfo/"):]
		}
	}
	return ""
}

// OperatorLocation is SystemTimezone as a *time.Location, UTC when unknown.
func OperatorLocation() *time.Location {
	if name := SystemTimezone(); name != "" {
		if l, err := time.LoadLocation(name); err == nil {
			return l
		}
	}
	return time.UTC
}
