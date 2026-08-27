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
	VerbAsk:         "ask your agent to do something, with all of its tools",
	VerbHarness:     "run a coding harness — shell, files and web research",
	VerbGateway:     "ask the main model directly",
	VerbHTTP:        "make HTTP requests",
	VerbTool:        "call KARMAX tools by name",
	VerbRecall:      "read your long-term memory",
	VerbRemember:    "WRITE to your long-term memory",
	VerbNotify:      "send you notifications",
	VerbPropose:     "ask for approval before acting — from you, or from anyone holding a role",
	VerbRemind:      "put reminders on your list",
	VerbSend:        "SEND MESSAGES AS YOU — to a WhatsApp contact, or into a shared channel",
	VerbCaseHistory: "read what has already happened on this piece of work",
	VerbCaseSay:     "speak in this piece of work's own thread, where its people can see it",
	VerbSleep:       "wait, and resume later",
	VerbLog:         "write to KARMAX's log",

	VerbCaseOpen:  "open or rejoin a case — a shared thread of work",
	VerbCaseGet:   "look up a case, without creating one",
	VerbCaseState: "move a case's state along, visible to everyone on it",
	VerbCaseLog:   "add a line to a case's shared history",
	VerbAwait:     "PARK the run — for as long as it takes — until a matching event happens",
	VerbForeach:   "repeat its steps once per item — whatever those steps do, they do it that many times",
	VerbSandbox:   "RUN CODE against a real repository in a container, and can push a branch",
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
			// The else branch, and a foreach body, are where a recipe would
			// hide the interesting verb, so both are walked too.
			walk(s.Else)
			walk(s.Steps)
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
			walk(s.Steps)
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
