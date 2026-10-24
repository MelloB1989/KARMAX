package config

import (
	"fmt"
	"regexp"
	"strings"
)

// A tool's class says what the KARMAX gateway does with a call to it: a read
// runs at once, a write waits for the orchestrator's approval, a deny is
// refused.
const (
	ToolRead  = "read"
	ToolWrite = "write"
	ToolDeny  = "deny"
)

// alwaysDenied are the tools no agent reaches through fleetd, whatever
// karmax.tools says: a shell, files and raw HTTP on KARMAX's host, its
// harness, tasks, schedules and proposals, the org's operations, writes to
// memory (the orchestrator alone writes it), credentials and secrets, and the
// fleet itself. A trailing * is a prefix.
var alwaysDenied = []string{
	"shell.exec", "file.*", "http.request", "harness.*", "task.*", "scheduler.*",
	"proposal.*", "orgops*", "memory.ingest", "memory.forget",
}

// deniedWords are parts of a name that deny a tool wherever they appear.
var deniedWords = []string{"credential", "secret", "fleet"}

// readVerbs name a tool that only reads, as the first word of its last part
// (google.mail.search, notion.query_database). Every other tool writes.
var readVerbs = map[string]bool{
	"list": true, "get": true, "read": true, "search": true, "view": true, "show": true,
	"find": true, "recall": true, "retrieve": true, "monitored": true, "lookup": true,
	"query": true, "history": true, "status": true, "schema": true, "describe": true,
	"count": true,
}

// toolName is a KARMAX tool's name: dotted parts, nothing that would make it
// another path or a query on KARMAX's API.
var toolName = regexp.MustCompile(`^[A-Za-z0-9_-]+(\.[A-Za-z0-9_-]+)*$`)

// ValidToolName reports whether a name could be a KARMAX tool's.
func ValidToolName(name string) bool { return toolName.MatchString(name) }

// globMatch matches a karmax.tools key: exact, or a prefix ending in *.
func globMatch(pattern, name string) bool {
	if p, ok := strings.CutSuffix(pattern, "*"); ok {
		return strings.HasPrefix(name, p)
	}
	return pattern == name
}

// AlwaysDenied reports whether a tool is one no entry in karmax.tools opens.
func AlwaysDenied(name string) bool {
	n := strings.ToLower(name)
	for _, p := range alwaysDenied {
		if globMatch(p, n) {
			return true
		}
	}
	for _, w := range deniedWords {
		if strings.Contains(n, w) {
			return true
		}
	}
	return false
}

// ToolClass is what the gateway does with a call to the named tool: the
// always-denied list first, then the most specific karmax.tools entry (an
// exact name, else the longest prefix*), then the name itself — a read verb
// reads, anything else writes. A name that is not a tool's is denied.
func (k Karmax) ToolClass(name string) string {
	if !ValidToolName(name) || AlwaysDenied(name) {
		return ToolDeny
	}
	if c, ok := k.Tools[name]; ok {
		return c
	}
	best, class := -1, ""
	for p, c := range k.Tools {
		if strings.HasSuffix(p, "*") && globMatch(p, name) && len(p) > best {
			best, class = len(p), c
		}
	}
	if best >= 0 {
		return class
	}
	parts := strings.Split(name, ".")
	if len(parts) > 1 {
		last := parts[len(parts)-1]
		first, _, _ := strings.Cut(last, "_")
		if readVerbs[last] || readVerbs[first] {
			return ToolRead
		}
	}
	return ToolWrite
}

// validateTools checks karmax.tools: each key a tool name or prefix*, each
// class read, write or deny, and no entry that would open an always-denied
// tool.
func (k Karmax) validateTools() error {
	for p, c := range k.Tools {
		if c != ToolRead && c != ToolWrite && c != ToolDeny {
			return fmt.Errorf("karmax: tools: %s: %q is not read, write or deny", p, c)
		}
		name := strings.TrimSuffix(p, "*")
		if name == "" || !ValidToolName(strings.TrimSuffix(name, ".")) {
			return fmt.Errorf("karmax: tools: %q is not a tool name or a prefix ending in *", p)
		}
		if c != ToolDeny && !strings.HasSuffix(p, "*") && AlwaysDenied(p) {
			return fmt.Errorf("karmax: tools: %s is always denied to agents; no entry opens it", p)
		}
	}
	return nil
}
