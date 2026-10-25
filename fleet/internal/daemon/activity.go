package daemon

import (
	"slices"
	"strings"
	"time"

	"github.com/MelloB1989/karmax/fleet/internal/fleetapi"
	"github.com/MelloB1989/karmax/fleet/internal/reconcile"
)

// ToolEvent is a worker's latest tool, from its telemetry (otel.tool_result
// or otel.tool_decision): the tool's name and, for Bash, the start of its
// command when the event carries one.
type ToolEvent struct {
	Tool, Command string
	At            time.Time
}

// thinkAfter is how long a working worker shows its last tool; after that,
// with no new one, it is thinking.
const thinkAfter = 20 * time.Second

// Activity is what a worker is doing from its state and its latest tool
// event: what its sprite acts out (fleetapi.Act*).
func Activity(row Row, last ToolEvent, now time.Time) fleetapi.Activity {
	if row.Unreachable {
		return fleetapi.Activity{Kind: fleetapi.ActDown}
	}
	switch reconcile.Phase(row.State) {
	case reconcile.Working:
	case reconcile.Waiting:
		return fleetapi.Activity{Kind: fleetapi.ActWaiting}
	case reconcile.Standby:
		a := fleetapi.Activity{Kind: fleetapi.ActIdle}
		if row.Idle != "" {
			a.Since = now.Add(-time.Duration(row.IdleS) * time.Second)
		}
		return a
	case reconcile.Cooling:
		return fleetapi.Activity{Kind: fleetapi.ActCooling, Since: row.Cooling}
	default: // down, disabled, code_missing, unreachable
		return fleetapi.Activity{Kind: fleetapi.ActDown}
	}
	a := fleetapi.Activity{Kind: fleetapi.ActThinking, Tool: last.Tool, Since: last.At}
	if last.Tool == "" || now.Sub(last.At) > thinkAfter {
		return a
	}
	switch last.Tool {
	case "Read", "Grep", "Glob", "WebFetch", "WebSearch":
		a.Kind = fleetapi.ActReading
	case "Edit", "MultiEdit", "Write", "NotebookEdit":
		a.Kind = fleetapi.ActEditing
	case "Bash":
		a.Kind = bashActivity(last.Command)
	default:
		a.Kind = fleetapi.ActRunning
	}
	return a
}

// testCommands are the commands that run a project's tests, as words.
var testCommands = [][]string{
	{"go", "test"}, {"npm", "test"}, {"pnpm", "test"}, {"cargo", "test"}, {"make", "test"},
	{"pytest"}, {"vitest"},
}

// bashActivity is what a Bash command does: pushing for a git push anywhere
// in it, testing for a test command, else running.
func bashActivity(cmd string) fleetapi.ActivityKind {
	kind := fleetapi.ActRunning
	for _, seg := range commandSegments(cmd) {
		if gitPush(seg) {
			return fleetapi.ActPushing
		}
		for i := range seg {
			for _, tc := range testCommands {
				if i+len(tc) <= len(seg) && slices.Equal(seg[i:i+len(tc)], tc) {
					kind = fleetapi.ActTesting
				}
			}
		}
	}
	return kind
}

// commandSegments splits a shell command into its simple commands' words:
// at &&, ||, ;, | and newlines, with quotes and parentheses taken as spaces
// (kbuild "npm ci && npm test" is two commands, the second a test).
func commandSegments(cmd string) [][]string {
	cmd = strings.NewReplacer(`"`, " ", `'`, " ", "(", " ", ")", " ", "`", " ").Replace(cmd)
	parts := strings.FieldsFunc(cmd, func(r rune) bool { return r == '&' || r == '|' || r == ';' || r == '\n' })
	out := make([][]string, 0, len(parts))
	for _, p := range parts {
		if w := strings.Fields(p); len(w) > 0 {
			out = append(out, w)
		}
	}
	return out
}

// gitPush reports whether a simple command is git push, past git's own
// options (git -C dir push).
func gitPush(words []string) bool {
	for i := 0; i < len(words); i++ {
		if words[i] != "git" {
			continue
		}
		j := i + 1
		for j < len(words) && strings.HasPrefix(words[j], "-") {
			if words[j] == "-C" || words[j] == "-c" {
				j++
			}
			j++
		}
		if j < len(words) && words[j] == "push" {
			return true
		}
	}
	return false
}
