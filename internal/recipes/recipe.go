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

	"github.com/robfig/cron/v3"
	"gopkg.in/yaml.v3"
)

// Verbs a step may use. Chosen to cover digests, watches, triage and reminders,
// which is the honest 80%.
const (
	VerbAsk      = "ask"      // the operator's agent, with tools and judgement
	VerbObserve  = "observe"  // the same agent with every way to speak withheld
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

	// Organisational verbs. A recipe working inside a company needs a thread of
	// work to belong to, a way to wait on other people, and somewhere to build.
	VerbCaseOpen    = "case.open"    // open or rejoin the case for a key
	VerbCaseGet     = "case.get"     // look one up without creating it
	VerbCaseState   = "case.state"   // move the work along
	VerbCaseLog     = "case.log"     // append to the case's history
	VerbCaseHistory = "case.history" // read what has already happened on it
	VerbCaseSay     = "case.say"     // speak in the case's own thread
	VerbAwait       = "await"        // park until a matching event arrives
	VerbForeach     = "foreach"      // run steps once per item
	VerbSandbox     = "sandbox"      // hand a coding task to a container
)

var verbs = []string{
	VerbAsk, VerbObserve, VerbHarness, VerbGateway, VerbHTTP, VerbTool, VerbRecall, VerbRemember,
	VerbNotify, VerbPropose, VerbRemind, VerbSend, VerbSleep, VerbLog,
	VerbCaseOpen, VerbCaseGet, VerbCaseState, VerbCaseLog, VerbCaseHistory, VerbCaseSay,
	VerbAwait, VerbForeach, VerbSandbox,
}

// Recipe is one YAML file.
type Recipe struct {
	Name    string  `yaml:"name"`
	Enabled *bool   `yaml:"enabled"`
	On      Trigger `yaml:"on"`
	// Steps is walked by hand (parseSteps) so each keeps its line number, and
	// is skipped here so the generic decode never sees it. It used to be
	// decoded twice: the first attempt failed on anything malformed with
	// "cannot unmarshal !!map into []recipes.Step" — a Go type name, shown to
	// someone writing YAML — and returned before the walker could say "steps
	// must be a list" and point at the line.
	Steps  []Step   `yaml:"-"`
	Grants []string `yaml:"grants"`

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

	// Steps is foreach's nested body — the one place a step's value is a list
	// of steps rather than a set of fields.
	Steps []Step
	// Match is await's payload matchers — the one place a step's value has a
	// field that is itself several field: value pairs rather than one scalar.
	Match map[string]string
	// In is foreach's item list when written as a literal YAML sequence rather
	// than a template that renders one; the template form lands in Args["in"]
	// like everything else.
	In []string

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
					name, v := val.Content[j], val.Content[j+1]
					if v.Kind == yaml.ScalarNode {
						s.Args[name.Value] = v.Value
						continue
					}
					// A nested block has no scalar value, so it would land here
					// as an empty string and the step would run as though the
					// field had never been written. Refused instead: silently
					// dropping half of what someone wrote is the worst way to
					// disagree with them — except the three fields below, which
					// are blocks on purpose (a match is several conditions, a
					// foreach body is a nested recipe).
					switch {
					case key == VerbAwait && name.Value == "match":
						m, err := decodeMatch(path, v)
						if err != nil {
							return s, err
						}
						s.Match = m
					case key == VerbForeach && name.Value == "steps":
						nested, err := decodeForeachSteps(path, v)
						if err != nil {
							return s, err
						}
						s.Steps = nested
					case key == VerbForeach && name.Value == "in":
						items, err := decodeForeachIn(path, v)
						if err != nil {
							return s, err
						}
						s.In = items
					default:
						return s, &Error{Path: path, Line: v.Line,
							Message: fmt.Sprintf("%q under %q is a block, and steps take plain values", name.Value, key),
							Fix:     nestedFix(key, name.Value)}
					}
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
	// Normalised in place, so the scheduler is handed the six-field form
	// regardless of which the author wrote.
	normalised, err := normaliseSchedule(r.Path, t.Schedule)
	if err != nil {
		return err
	}
	r.On.Schedule = normalised
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
		// A foreach body is validated too, or a mistake in it is invisible
		// until the recipe actually runs and hits that item.
		if err := validateSteps(path, s.Steps); err != nil {
			return err
		}
	}
	return nil
}

// required names the fields each map-form verb cannot do without.
var required = map[string][]string{
	VerbHTTP:    {"url"},
	VerbTool:    {"name"},
	VerbNotify:  {"title"},
	VerbPropose: {"title", "action"},
	VerbRemind:  {"title"},
	// 'to' is not listed here: it is required unless 'channel' stands in for
	// it, which is a choice between two fields rather than one field always
	// being there — checked by hand below.
	VerbSend:     {"text"},
	VerbRemember: {"fact"},

	VerbCaseOpen:    {"key", "title"},
	VerbCaseGet:     {"key"},
	VerbCaseState:   {"case", "state"},
	VerbCaseLog:     {"case", "kind", "payload"},
	VerbCaseHistory: {"case"},
	VerbCaseSay:     {"case", "text"},
	VerbAwait:       {"event"},
	VerbSandbox:     {"repo", "branch", "task"},
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
	// 'to' is required UNLESS 'channel' names the target instead — a bare
	// 'to: "#eng"' also works, since that IS the channel.
	if s.Verb == VerbSend && strings.TrimSpace(s.Args["to"]) == "" && strings.TrimSpace(s.Args["channel"]) == "" {
		return &Error{Path: path, Line: s.Line, Message: "\"send\" needs \"to\" or \"channel\"",
			Fix: "give a WhatsApp target as 'to', or a channel to post into as 'channel' (or 'to' — \"#eng\" works as either)"}
	}
	if s.Verb == VerbForeach {
		if strings.TrimSpace(s.Args["in"]) == "" && len(s.In) == 0 {
			return &Error{Path: path, Line: s.Line, Message: "\"foreach\" needs \"in\"",
				Fix: "give it a list ('in: [\"a\", \"b\"]') or a template that renders one ('in: \"{{ .tickets }}\"')"}
		}
		if strings.TrimSpace(s.Args["as"]) == "" {
			return &Error{Path: path, Line: s.Line, Message: "\"foreach\" needs \"as\"",
				Fix: "name each item, e.g. 'as: t', so 'steps:' can refer to {{ .t }}"}
		}
		if len(s.Steps) == 0 {
			return &Error{Path: path, Line: s.Line, Message: "\"foreach\" needs \"steps\"",
				Fix: "add a nested 'steps:' list — the body to run once per item"}
		}
	}
	return nil
}

func storesResult(verb string) bool {
	switch verb {
	case VerbAsk, VerbObserve, VerbHarness, VerbGateway, VerbHTTP, VerbTool, VerbRecall,
		VerbCaseOpen, VerbCaseGet, VerbCaseHistory, VerbAwait, VerbSandbox:
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

// decodeMatch reads await's match: block — the fields a payload must equal.
func decodeMatch(path string, v *yaml.Node) (map[string]string, error) {
	if v.Kind != yaml.MappingNode {
		return nil, &Error{Path: path, Line: v.Line,
			Message: "\"match\" under \"await\" must be a set of field: value pairs",
			Fix:     "write it as:\n    await:\n      event: ...\n      match: { key: value, status: value }"}
	}
	out := map[string]string{}
	for i := 0; i+1 < len(v.Content); i += 2 {
		name, val := v.Content[i], v.Content[i+1]
		if val.Kind != yaml.ScalarNode {
			return nil, &Error{Path: path, Line: val.Line,
				Message: fmt.Sprintf("%q under %q is a block, and a match compares one value", name.Value, "match"),
				Fix:     "give it a single value to compare against"}
		}
		out[name.Value] = val.Value
	}
	return out, nil
}

// decodeForeachSteps reads foreach's nested steps: — a recipe inside a recipe.
func decodeForeachSteps(path string, v *yaml.Node) ([]Step, error) {
	if v.Kind != yaml.SequenceNode {
		return nil, &Error{Path: path, Line: v.Line, Message: "\"steps\" under \"foreach\" must be a list",
			Fix: "each nested step starts with '- ', just like the recipe's own steps:"}
	}
	return decodeStepList(path, v)
}

// decodeForeachIn reads a literal in: list. The template form ("{{ .x }}")
// never reaches here — it is a scalar, so it is caught before this switch.
func decodeForeachIn(path string, v *yaml.Node) ([]string, error) {
	if v.Kind != yaml.SequenceNode {
		return nil, &Error{Path: path, Line: v.Line, Message: "\"in\" under \"foreach\" must be a list",
			Fix: "write a literal list ('in: [\"a\", \"b\"]'), or a template that renders one ('in: \"{{ .tickets }}\"')"}
	}
	out := make([]string, 0, len(v.Content))
	for _, item := range v.Content {
		if item.Kind != yaml.ScalarNode {
			return nil, &Error{Path: path, Line: item.Line, Message: "each item in a literal \"in\" list must be plain text",
				Fix: "list plain values, or move the structure into an earlier step and pass it as a rendered template"}
		}
		out = append(out, item.Value)
	}
	return out, nil
}

// nestedFix suggests the flat form for a field someone nested.
//
// http headers are the case this exists for: "headers:" with keys under it is
// the obvious thing to write and is not how they are spelled, so the error
// names the spelling rather than saying "no blocks".
func nestedFix(verb, field string) string {
	if verb == VerbHTTP && (field == "headers" || field == "header") {
		return "write each header flat:\n    - http:\n        url: ...\n        header.Authorization: Bearer ..."
	}
	return fmt.Sprintf("give %q a single value on one line", field)
}

// normaliseSchedule checks a schedule and returns the form the scheduler runs.
//
// Checked at parse time, with the same parser the scheduler uses, because
// otherwise a bad expression is accepted by `karmax recipe check`, reported as
// valid, and then fails when the daemon registers the job — one WARN in a log
// nobody is reading. The author finds out by noticing, weeks later, that their
// recipe has never run. Two of this package's own test recipes were written
// with a schedule that could never have fired.
//
// FIVE-field crontab is accepted and normalised. KARMAX's scheduler takes six
// fields because loops sometimes need seconds, but five is the syntax the whole
// world writes and every example anyone will copy from. Rejecting it to be
// consistent with an internal detail would be making the operator pay for our
// scheduler's extra column.
func normaliseSchedule(path, spec string) (string, error) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return "", nil
	}
	if _, err := cronParser.Parse(spec); err == nil {
		return spec, nil
	}
	// Exactly five fields is standard crontab; seconds are simply absent.
	if fields := strings.Fields(spec); len(fields) == 5 {
		withSeconds := "0 " + strings.Join(fields, " ")
		if _, err := cronParser.Parse(withSeconds); err == nil {
			return withSeconds, nil
		}
	}
	_, err := cronParser.Parse(spec)
	return "", &Error{Path: path, Line: 1,
		Message: fmt.Sprintf("the schedule %q is not one the scheduler can run: %v", spec, err),
		Fix: "cron takes five fields, or six if you need seconds:\n" +
			"    \"0 9 * * *\"      every day at 09:00\n" +
			"    \"0 0 9 * * *\"    the same thing, with seconds\n" +
			"    \"0 */15 * * * *\" every 15 minutes\n" +
			"  or a shorthand: \"@every 45m\", \"@daily\"",
	}
}

// cronParser matches scheduler.New's cron.WithSeconds(), so the two cannot
// disagree about what a valid schedule is.
var cronParser = cron.NewParser(
	cron.Second | cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor,
)
