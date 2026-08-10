package recipes

import (
	"fmt"
	"sort"
	"strings"
)

// What a recipe would be allowed to do, in plain English.
//
// A recipe is not signed and not sandboxed, because it is not code — it is data
// KARMAX interprets using its own tools, under the same Broker every other
// caller passes. That makes the verb list the whole of its reach, and it means
// an operator CAN be shown what installing one means, which the tier would
// otherwise be missing entirely.

var verbDescriptions = map[string]string{
	VerbAsk:      "ask your agent to do something, with all of its tools",
	VerbHarness:  "run a coding harness — shell, files and web research",
	VerbGateway:  "ask the main model directly",
	VerbHTTP:     "make HTTP requests",
	VerbTool:     "call KARMAX tools by name",
	VerbRecall:   "read your long-term memory",
	VerbRemember: "WRITE to your long-term memory",
	VerbNotify:   "send you notifications",
	VerbPropose:  "ask for your approval before acting",
	VerbRemind:   "put reminders on your list",
	VerbSend:     "SEND MESSAGES AS YOU",
	VerbSleep:    "wait, and resume later",
	VerbLog:      "write to KARMAX's log",
}

// Describe renders what a recipe does, for the operator to approve.
func Describe(r *Recipe) []string {
	if r == nil {
		return nil
	}
	seen := map[string]bool{}
	var out []string

	var walk func([]Step)
	walk = func(steps []Step) {
		for _, s := range steps {
			if d, ok := verbDescriptions[s.Verb]; ok && !seen[s.Verb] {
				seen[s.Verb] = true
				out = append(out, d)
			}
			// The else branch of a conditional step is where a recipe would
			// hide the interesting verb, so it is walked too.
			walk(s.Else)
		}
	}
	walk(r.Steps)
	sort.Strings(out)

	// Named tools are listed individually. "call KARMAX tools by name" is not
	// something anybody can weigh; "call the tool whatsapp.send" is.
	for _, name := range namedTools(r) {
		out = append(out, "call the tool "+name)
	}
	for _, g := range r.Grants {
		out = append(out, "hold the grant "+g)
	}
	if len(out) == 0 {
		out = append(out, "nothing — it has no steps")
	}
	return out
}

// namedTools finds the tools a recipe's `tool:` steps name.
func namedTools(r *Recipe) []string {
	seen := map[string]bool{}
	var out []string
	var walk func([]Step)
	walk = func(steps []Step) {
		for _, s := range steps {
			if s.Verb == VerbTool {
				name := strings.TrimSpace(s.Args["name"])
				if name == "" {
					name = strings.TrimSpace(s.Text)
				}
				if name != "" && !seen[name] {
					seen[name] = true
					out = append(out, name)
				}
			}
			walk(s.Else)
		}
	}
	walk(r.Steps)
	sort.Strings(out)
	return out
}

// Summary is a one-line description of a recipe's shape, for listings.
func Summary(r *Recipe) string {
	if r == nil {
		return ""
	}
	return fmt.Sprintf("%d step(s)", len(r.Steps))
}
