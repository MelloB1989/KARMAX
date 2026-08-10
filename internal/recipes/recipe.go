// Package recipes is the tier for people who do not write Go.
//
// One YAML file in ~/.karmax/recipes/, no install, no rebuild, no sandbox —
// because a tier with an install step is a tier nobody uses. A recipe compiles
// to a loopkit.Loop, so it inherits durable runs, retries, step checkpointing
// and timers rather than reimplementing any of them.
package recipes

import (
	"fmt"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Verbs a step may use. Chosen to cover digests, watches, triage and reminders,
// which is the honest 80%.
const (
	VerbAsk      = "ask"      // the operator's agent, with tools and judgement
	VerbHarness  = "harness"  // a coding harness, for research and shell work
	VerbGateway  = "gateway"  // the main model, no agent loop — cheapest
	VerbHTTP     = "http"     // fetch something
	VerbTool     = "tool"     // call a KARMAX tool by name
	VerbRecall   = "recall"   // read long-term memory
	VerbRemember = "remember" // write long-term memory
	VerbNotify   = "notify"   // tell the operator
	VerbPropose  = "propose"  // ask the operator to approve an action
	VerbRemind   = "remind"   // put something on the operator's list
	VerbSend     = "send"     // message someone through WhatsApp
	VerbSleep    = "sleep"    // durable wait; the run resumes later
	VerbLog      = "log"      // write a line to KARMAX's log
)

var verbs = []string{
	VerbAsk, VerbHarness, VerbGateway, VerbHTTP, VerbTool, VerbRecall, VerbRemember,
	VerbNotify, VerbPropose, VerbRemind, VerbSend, VerbSleep, VerbLog,
}

// Recipe is one YAML file.
type Recipe struct {
	Name    string   `yaml:"name"`
	Enabled *bool    `yaml:"enabled"`
	On      Trigger  `yaml:"on"`
	Steps   []Step   `yaml:"steps"`
	Grants  []string `yaml:"grants"`

	// Path is where it was loaded from, for error messages.
	Path string `yaml:"-"`
}

// Trigger says what starts a recipe.
type Trigger struct {
	Event    string `yaml:"event"`
	Schedule string `yaml:"schedule"`
	Webhook  string `yaml:"webhook"`
	Manual   bool   `yaml:"manual"`
}

// Step is one action. Exactly one verb per step, so the file reads top to
// bottom and there is never a question of what runs first.
type Step struct {
	Verb string
	// Args is the verb's value: a string for the simple verbs, a map for the
	// ones with several fields.
	Args map[string]string
	// Text is the verb's value when it was given as a plain string.
	Text string

	As   string
	When string
	Else []Step

	// Line is where this step starts in the file, for errors that point at
	// something rather than describing it.
	Line int
}

// IsEnabled reports whether the recipe should run; absent means yes.
func (r *Recipe) IsEnabled() bool { return r.Enabled == nil || *r.Enabled }

// Error is a problem with a recipe, located.
type Error struct {
	Path    string
	Line    int
	Message string
	// Fix is what to do about it, because a schema dump is not help.
	Fix string
}

func (e *Error) Error() string {
	s := fmt.Sprintf("%s:%d: %s", e.Path, e.Line, e.Message)
	if e.Fix != "" {
		s += "\n  try: " + e.Fix
	}
	return s
}

// Parse reads one recipe and validates it.
func Parse(path string, data []byte) (*Recipe, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, &Error{Path: path, Line: yamlLine(err), Message: cleanYAMLError(err),
			Fix: "check the indentation — YAML lists need '- ' and nested keys need two more spaces"}
	}
	if len(doc.Content) == 0 {
		return nil, &Error{Path: path, Line: 1, Message: "the file is empty"}
	}
	root := doc.Content[0]

	r := &Recipe{Path: path}
	if err := root.Decode(r); err != nil {
		return nil, &Error{Path: path, Line: root.Line, Message: cleanYAMLError(err)}
	}
	steps, err := parseSteps(path, root)
	if err != nil {
		return nil, err
	}
	r.Steps = steps

	if err := r.validate(); err != nil {
		return nil, err
	}
	return r, nil
}

// parseSteps walks the YAML tree so each step keeps its line number.
func parseSteps(path string, root *yaml.Node) ([]Step, error) {
	seq := childNode(root, "steps")
	if seq == nil {
		return nil, &Error{Path: path, Line: root.Line, Message: "no steps",
			Fix: "add a 'steps:' list — see `karmax recipe example`"}
	}
	return decodeStepList(path, seq)
}

func decodeStepList(path string, seq *yaml.Node) ([]Step, error) {
	if seq.Kind != yaml.SequenceNode {
		return nil, &Error{Path: path, Line: seq.Line, Message: "steps must be a list",
			Fix: "each step starts with '- ' on its own line"}
	}
	var out []Step
	for _, item := range seq.Content {
		s, err := decodeStep(path, item)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, nil
}

func decodeStep(path string, node *yaml.Node) (Step, error) {
	s := Step{Line: node.Line}
	if node.Kind != yaml.MappingNode {
		return s, &Error{Path: path, Line: node.Line, Message: "a step must be a mapping",
			Fix: "write it as '- notify: ...' rather than a bare value"}
	}

	var found []string
	for i := 0; i+1 < len(node.Content); i += 2 {
		key, val := node.Content[i].Value, node.Content[i+1]
		switch key {
		case "as":
			s.As = val.Value
		case "when":
			s.When = val.Value
		case "else":
			nested, err := decodeStepList(path, val)
			if err != nil {
				return s, err
			}
			s.Else = nested
		default:
			if !isVerb(key) {
				return s, &Error{Path: path, Line: node.Line,
					Message: fmt.Sprintf("unknown step %q", key),
					Fix:     "the verbs are " + strings.Join(verbs, ", ")}
			}
			found = append(found, key)
			s.Verb = key
			switch val.Kind {
			case yaml.ScalarNode:
				s.Text = val.Value
			case yaml.MappingNode:
				s.Args = map[string]string{}
				for j := 0; j+1 < len(val.Content); j += 2 {
					s.Args[val.Content[j].Value] = val.Content[j+1].Value
				}
			default:
				return s, &Error{Path: path, Line: val.Line,
					Message: fmt.Sprintf("%q needs text or a set of fields", key)}
			}
		}
	}

	if len(found) == 0 {
		return s, &Error{Path: path, Line: node.Line, Message: "this step has no action",
			Fix: "add one of " + strings.Join(verbs, ", ")}
	}
	if len(found) > 1 {
		sort.Strings(found)
		return s, &Error{Path: path, Line: node.Line,
			Message: fmt.Sprintf("this step does %d things at once (%s)", len(found), strings.Join(found, " and ")),
			Fix:     "split it into one step per action, so the order is unambiguous"}
	}
	return s, nil
}

func (r *Recipe) validate() error {
	if strings.TrimSpace(r.Name) == "" {
		return &Error{Path: r.Path, Line: 1, Message: "no name",
			Fix: "add 'name: something-descriptive' at the top"}
	}
	if strings.ContainsAny(r.Name, " /\\") {
		return &Error{Path: r.Path, Line: 1,
			Message: fmt.Sprintf("the name %q contains a space or a slash", r.Name),
			Fix:     "use-hyphens-like-this"}
	}
	t := r.On
	if t.Event == "" && t.Schedule == "" && t.Webhook == "" && !t.Manual {
		return &Error{Path: r.Path, Line: 1, Message: "nothing triggers this recipe",
			Fix: "add an 'on:' block with event, schedule, webhook, or manual: true"}
	}
	if len(r.Steps) == 0 {
		return &Error{Path: r.Path, Line: 1, Message: "no steps"}
	}
	return validateSteps(r.Path, r.Steps)
}

func validateSteps(path string, steps []Step) error {
	for _, s := range steps {
		if err := s.validate(path); err != nil {
			return err
		}
		if err := validateSteps(path, s.Else); err != nil {
			return err
		}
	}
	return nil
}

// required names the fields each map-form verb cannot do without.
var required = map[string][]string{
	VerbHTTP:     {"url"},
	VerbTool:     {"name"},
	VerbNotify:   {"title"},
	VerbPropose:  {"title", "action"},
	VerbRemind:   {"title"},
	VerbSend:     {"to", "text"},
	VerbRemember: {"fact"},
}

func (s Step) validate(path string) error {
	if s.Else != nil && s.When == "" {
		return &Error{Path: path, Line: s.Line, Message: "'else' with no 'when'",
			Fix: "add a 'when:' to the same step, or drop the else"}
	}
	if s.As != "" && !storesResult(s.Verb) {
		return &Error{Path: path, Line: s.Line,
			Message: fmt.Sprintf("%q produces nothing to bind with 'as'", s.Verb),
			Fix:     "remove 'as', or use a verb that returns something (ask, harness, gateway, http, tool, recall)"}
	}
	for _, key := range required[s.Verb] {
		if s.Args == nil || strings.TrimSpace(s.Args[key]) == "" {
			return &Error{Path: path, Line: s.Line,
				Message: fmt.Sprintf("%q needs %q", s.Verb, key),
				Fix:     fmt.Sprintf("write it as:\n    - %s:\n        %s: ...", s.Verb, key)}
		}
	}
	if s.Verb == VerbSleep && s.Text == "" && (s.Args == nil || s.Args["for"] == "") {
		return &Error{Path: path, Line: s.Line, Message: "sleep needs a duration",
			Fix: "'- sleep: 72h' — the run is parked and resumes later, so hours and days are fine"}
	}
	return nil
}

func storesResult(verb string) bool {
	switch verb {
	case VerbAsk, VerbHarness, VerbGateway, VerbHTTP, VerbTool, VerbRecall:
		return true
	}
	return false
}

func isVerb(k string) bool {
	for _, v := range verbs {
		if v == k {
			return true
		}
	}
	return false
}

func childNode(m *yaml.Node, key string) *yaml.Node {
	if m.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}

// yamlLine digs the line out of a yaml error, which reports it in the message.
func yamlLine(err error) int {
	var line int
	if n, _ := fmt.Sscanf(err.Error(), "yaml: line %d", &line); n == 1 {
		return line
	}
	return 1
}

func cleanYAMLError(err error) string {
	msg := err.Error()
	msg = strings.TrimPrefix(msg, "yaml: ")
	if i := strings.Index(msg, ": "); i > 0 && strings.HasPrefix(msg, "line ") {
		msg = msg[i+2:]
	}
	return msg
}
