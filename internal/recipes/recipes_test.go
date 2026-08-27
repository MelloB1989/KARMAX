package recipes

import (
	"context"
	"fmt"
	"go/format"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MelloB1989/karmax/pkg/loopkit"
)

func parse(t *testing.T, yaml string) (*Recipe, error) {
	t.Helper()
	return Parse("test.yaml", []byte(yaml))
}

func mustParse(t *testing.T, yaml string) *Recipe {
	t.Helper()
	r, err := parse(t, yaml)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return r
}

func TestAWorkingRecipeParses(t *testing.T) {
	r := mustParse(t, `
name: morning-digest
on:
  schedule: "0 9 * * *"
steps:
  - recall: "what is on today"
    as: ctx
  - notify:
      title: "Good morning"
      body: "{{ .ctx }}"
`)
	if r.Name != "morning-digest" || len(r.Steps) != 2 {
		t.Fatalf("parsed %+v", r)
	}
	if r.Steps[0].As != "ctx" || r.Steps[0].Verb != VerbRecall {
		t.Errorf("step 0 = %+v", r.Steps[0])
	}
	if r.Steps[1].Args["title"] != "Good morning" {
		t.Errorf("step 1 args = %+v", r.Steps[1].Args)
	}
	if !r.IsEnabled() {
		t.Error("a recipe with no 'enabled' should be enabled")
	}
}

// Errors are the whole product for this tier: the audience cannot read a
// schema dump, so every message must point at a line and suggest a fix.
func TestErrorsPointAtALineAndSuggestAFix(t *testing.T) {
	cases := []struct {
		name, yaml, wants string
		line              int
	}{
		{
			name:  "no trigger",
			yaml:  "name: x\nsteps:\n  - log: hello\n",
			wants: "nothing triggers",
		},
		{
			name:  "no name",
			yaml:  "on:\n  manual: true\nsteps:\n  - log: hello\n",
			wants: "no name",
		},
		{
			name:  "a space in the name",
			yaml:  "name: my recipe\non:\n  manual: true\nsteps:\n  - log: hi\n",
			wants: "use-hyphens-like-this",
		},
		{
			name:  "two verbs in one step",
			yaml:  "name: x\non:\n  manual: true\nsteps:\n  - log: hi\n    notify:\n      title: t\n",
			wants: "2 things at once",
			line:  5,
		},
		{
			name:  "an unknown verb",
			yaml:  "name: x\non:\n  manual: true\nsteps:\n  - teleport: away\n",
			wants: "unknown step",
			line:  5,
		},
		{
			name:  "a missing required field",
			yaml:  "name: x\non:\n  manual: true\nsteps:\n  - send:\n      to: someone\n",
			wants: `"send" needs "text"`,
			line:  5,
		},
		{
			name:  "else with no when",
			yaml:  "name: x\non:\n  manual: true\nsteps:\n  - log: hi\n    else:\n      - log: bye\n",
			wants: "'else' with no 'when'",
			line:  5,
		},
		{
			name:  "binding something that returns nothing",
			yaml:  "name: x\non:\n  manual: true\nsteps:\n  - notify:\n      title: t\n    as: v\n",
			wants: "produces nothing to bind",
			line:  5,
		},
		{
			name:  "sleep with no duration",
			yaml:  "name: x\non:\n  manual: true\nsteps:\n  - sleep:\n      unrelated: 1\n",
			wants: "sleep needs a duration",
			line:  5,
		},
		{
			name:  "no steps at all",
			yaml:  "name: x\non:\n  manual: true\n",
			wants: "no steps",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parse(t, tc.yaml)
			if err == nil {
				t.Fatal("accepted an invalid recipe")
			}
			re, ok := err.(*Error)
			if !ok {
				t.Fatalf("error is not located: %v", err)
			}
			// Either half may carry it: the message says what is wrong, the fix
			// says what to do, and the author reads both.
			if !strings.Contains(re.Message, tc.wants) && !strings.Contains(re.Fix, tc.wants) {
				t.Errorf("neither message %q nor fix %q mentions %q", re.Message, re.Fix, tc.wants)
			}
			if tc.line != 0 && re.Line != tc.line {
				t.Errorf("points at line %d, want %d", re.Line, tc.line)
			}
			if re.Fix == "" && tc.name != "no steps" {
				t.Error("no suggested fix — a message with no fix is a schema dump")
			}
		})
	}
}

func TestADryRunPerformsNothing(t *testing.T) {
	r := mustParse(t, `
name: noisy
on:
  manual: true
steps:
  - remember:
      fact: "something true"
  - send:
      to: "+911234567890"
      text: "hello"
  - notify:
      title: "done"
      body: "all of it"
`)
	d := NewDryRun(loopkit.Trigger{Kind: loopkit.TriggerManual})
	if err := Run(context.Background(), r, d); err != nil {
		t.Fatal(err)
	}
	report := d.Report()
	for _, want := range []string{"REMEMBER", "SEND WhatsApp to +911234567890", "NOTIFY"} {
		if !strings.Contains(report, want) {
			t.Errorf("report missing %q:\n%s", want, report)
		}
	}
	if len(d.Actions) != 3 {
		t.Errorf("recorded %d actions, want 3", len(d.Actions))
	}
}

func TestBindingsFlowIntoLaterSteps(t *testing.T) {
	r := mustParse(t, `
name: chained
on:
  manual: true
steps:
  - ask: "what is the plan"
    as: plan
  - notify:
      title: "Plan"
      body: "{{ .plan }}"
`)
	d := NewDryRun(loopkit.Trigger{Kind: loopkit.TriggerManual})
	if err := Run(context.Background(), r, d); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(d.Report(), "the agent's answer") {
		t.Errorf("the binding did not reach the later step:\n%s", d.Report())
	}
}

func TestTriggerPayloadIsAvailableAsBindings(t *testing.T) {
	r := mustParse(t, `
name: uses-payload
on:
  event: github.event
steps:
  - notify:
      title: "{{ .repo }} #{{ .number }}"
      body: "by {{ .actor }}"
`)
	d := NewDryRun(loopkit.Trigger{Kind: loopkit.TriggerEvent, Payload: map[string]any{
		"repo": "o/r", "number": 7, "actor": "someone",
	}})
	if err := Run(context.Background(), r, d); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(d.Report(), "o/r #7") || !strings.Contains(d.Report(), "by someone") {
		t.Errorf("payload not bound:\n%s", d.Report())
	}
}

func TestWhenAndElseChooseOneBranch(t *testing.T) {
	yaml := `
name: branching
on:
  manual: true
steps:
  - when: "{{ .flag }}"
    notify:
      title: "it was set"
    else:
      - notify:
          title: "it was not"
`
	r := mustParse(t, yaml)

	set := NewDryRun(loopkit.Trigger{Kind: loopkit.TriggerManual, Payload: map[string]any{"flag": "yes"}})
	if err := Run(context.Background(), r, set); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(set.Report(), "it was set") || strings.Contains(set.Report(), "it was not") {
		t.Errorf("wrong branch when set:\n%s", set.Report())
	}

	unset := NewDryRun(loopkit.Trigger{Kind: loopkit.TriggerManual})
	if err := Run(context.Background(), r, unset); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(unset.Report(), "it was not") || strings.Contains(unset.Report(), "it was set") {
		t.Errorf("wrong branch when unset:\n%s", unset.Report())
	}
}

func TestALongSleepParksTheRunRatherThanBlocking(t *testing.T) {
	r := mustParse(t, `
name: patient
on:
  manual: true
steps:
  - log: "starting"
  - sleep: 72h
  - notify:
      title: "three days later"
`)
	d := NewDryRun(loopkit.Trigger{Kind: loopkit.TriggerManual})
	if err := Run(context.Background(), r, d); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(d.Report(), "survives a restart") {
		t.Errorf("a 72h sleep did not become a durable timer:\n%s", d.Report())
	}
}

func TestABrokenRecipeDoesNotStopTheOthers(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("good.yaml", "name: good\non:\n  manual: true\nsteps:\n  - log: fine\n")
	write("bad.yaml", "name: bad\nsteps:\n  - log: no trigger\n")
	write("notes.txt", "not a recipe")

	loaded := LoadAll(dir)
	if len(loaded) != 2 {
		t.Fatalf("loaded %d files, want 2 (the .txt should be ignored)", len(loaded))
	}
	valid := Valid(loaded)
	if len(valid) != 1 || valid[0].Name != "good" {
		t.Errorf("valid recipes = %+v", valid)
	}
}

func TestADisabledRecipeIsNotRun(t *testing.T) {
	r := mustParse(t, "name: off\nenabled: false\non:\n  manual: true\nsteps:\n  - log: hi\n")
	if r.IsEnabled() {
		t.Error("enabled: false was ignored")
	}
	if len(Valid([]Loaded{{Recipe: r}})) != 0 {
		t.Error("a disabled recipe was returned as valid")
	}
}

func TestEjectProducesGoThatMatchesTheRecipe(t *testing.T) {
	r := mustParse(t, `
name: digest
on:
  schedule: "0 9 * * *"
steps:
  - recall: "today"
    as: ctx
  - notify:
      title: "Morning"
      body: "{{ .ctx }}"
`)
	code := Eject(r, "")
	for _, want := range []string{
		"package digest",
		`loopkit.Register`,
		`Name: "digest"`,
		// The NORMALISED schedule: the recipe was written with five-field
		// crontab and the ejected Go must carry what the scheduler runs, or the
		// loop it becomes fires at a different time than the recipe did.
		`loopkit.Cron("0 0 9 * * *")`,
		"k.Recall",
		"k.Notify",
		// Step names must survive, or an in-flight run resumes at the wrong place.
		`k.Step("0-recall"`,
		`k.Once("1-notify"`,
	} {
		if !strings.Contains(code, want) {
			t.Errorf("generated code missing %q:\n%s", want, code)
		}
	}
}

// caseGetKit answers CaseGet with a fixed found/not-found result, everything
// else via DryRun. It exists to test the ONE thing DryRun's own fake CaseGet
// papers over: what a real "not found" binds to.
type caseGetKit struct {
	*DryRun
	found bool
}

func (k *caseGetKit) CaseGet(key string) (loopkit.Case, bool, error) {
	if !k.found {
		return loopkit.Case{}, false, nil
	}
	return loopkit.Case{ID: "case-1", Key: key, State: "open"}, true, nil
}

// case.get on a key nobody opened is not an error — it binds a Case whose
// fields are all empty, so 'when: "{{ .c.id }}"' reads as false and a recipe
// can branch on "does this exist yet" without case.get ever failing the run.
func TestCaseGetNotFoundBindsEmptyRatherThanErroring(t *testing.T) {
	r := mustParse(t, `
name: x
on:
  manual: true
steps:
  - case.get: { key: "nope" }
    as: c
  - when: "{{ .c.id }}"
    notify: { title: "FOUND-BRANCH" }
    else:
      - notify: { title: "MISSING-BRANCH" }
`)
	k := &caseGetKit{DryRun: NewDryRun(loopkit.Trigger{Kind: loopkit.TriggerManual}), found: false}
	if err := Run(context.Background(), r, k); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(k.Report(), "MISSING-BRANCH") || strings.Contains(k.Report(), "FOUND-BRANCH") {
		t.Errorf("a not-found case.get did not read as empty:\n%s", k.Report())
	}

	k2 := &caseGetKit{DryRun: NewDryRun(loopkit.Trigger{Kind: loopkit.TriggerManual}), found: true}
	if err := Run(context.Background(), r, k2); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(k2.Report(), "FOUND-BRANCH") {
		t.Errorf("a found case.get did not read as present:\n%s", k2.Report())
	}
}

// idRecordingKit records every checkpoint name k.Step and k.Once are called
// with, so a foreach's iterations can be checked for the one thing that
// matters: no two of them ever ask for the same checkpoint.
type idRecordingKit struct {
	*DryRun
	ids []string
}

func (k *idRecordingKit) Step(name string, fn func() (string, error)) (string, error) {
	k.ids = append(k.ids, name)
	return k.DryRun.Step(name, fn)
}

func (k *idRecordingKit) Once(name string, fn func() error) error {
	k.ids = append(k.ids, name)
	return k.DryRun.Once(name, fn)
}

// The bug this package's build contract calls out by name: without the
// iteration index folded into each nested step's checkpoint id, every pass
// through a foreach reuses iteration zero's ids — so a retry after item
// three fails would find item one's steps already "done" and skip straight
// past the rest of the list.
func TestForeachIterationsGetDistinctCheckpointIDs(t *testing.T) {
	r := mustParse(t, `
name: x
on:
  manual: true
steps:
  - foreach:
      in: '["a1", "a2", "a3"]'
      as: t
      steps:
        - ask: "summarise {{ .t }}"
          as: out
        - recall: "notes on {{ .t }}"
          as: notes
`)
	k := &idRecordingKit{DryRun: NewDryRun(loopkit.Trigger{Kind: loopkit.TriggerManual})}
	if err := Run(context.Background(), r, k); err != nil {
		t.Fatal(err)
	}
	if len(k.ids) != 6 {
		t.Fatalf("recorded %d checkpoint ids, want 6 (3 items * 2 steps): %v", len(k.ids), k.ids)
	}
	seen := map[string]bool{}
	for _, id := range k.ids {
		if seen[id] {
			t.Errorf("checkpoint id %q reused across iterations: %v", id, k.ids)
		}
		seen[id] = true
	}
}

// foreach accepts a literal in: list too, not only a rendered JSON template.
func TestForeachOverALiteralInList(t *testing.T) {
	r := mustParse(t, `
name: x
on:
  manual: true
steps:
  - foreach:
      in: ["x", "y"]
      as: item
      steps:
        - log: "item {{ .item }}"
`)
	d := NewDryRun(loopkit.Trigger{Kind: loopkit.TriggerManual})
	if err := Run(context.Background(), r, d); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"item x", "item y"} {
		if !strings.Contains(d.Report(), want) {
			t.Errorf("report missing %q:\n%s", want, d.Report())
		}
	}
}

// foreach's in: also accepts a JSON array bound by an earlier step — the
// common shape, since the list usually comes from an http/tool call rather
// than being written into the recipe by hand.
func TestForeachOverAJSONArrayFromAnEarlierBinding(t *testing.T) {
	r := mustParse(t, `
name: x
on:
  event: webhook
steps:
  - foreach:
      in: "{{ .tickets }}"
      as: t
      steps:
        - log: "ticket {{ .t }}"
`)
	d := NewDryRun(loopkit.Trigger{Kind: loopkit.TriggerEvent, Payload: map[string]any{
		"tickets": `["OPS-1","OPS-2"]`,
	}})
	if err := Run(context.Background(), r, d); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"ticket OPS-1", "ticket OPS-2"} {
		if !strings.Contains(d.Report(), want) {
			t.Errorf("report missing %q:\n%s", want, d.Report())
		}
	}
}

// suspendingKit forces Await to park the run, the way the real Kit does when
// a waiter is armed and nothing has matched it yet.
type suspendingKit struct {
	*DryRun
}

func (k *suspendingKit) Await(_ context.Context, id string, spec loopkit.AwaitSpec) (map[string]any, error) {
	return nil, fmt.Errorf("%w: %s", loopkit.ErrSuspended, spec.Event)
}

// A suspended run is not a failed one: runSteps must hand the error straight
// up rather than swallowing or reclassifying it, so the runtime — which owns
// the retry/dead-letter decision — can tell loopkit.Suspended(err) is true
// and skip both.
func TestSuspensionPropagatesUnrecognisableAsAFailure(t *testing.T) {
	r := mustParse(t, `
name: parks
on:
  manual: true
steps:
  - notify: { title: "before" }
  - await:
      event: jira.issue.updated
      match: { status: Done }
    as: moved
  - notify: { title: "after — must not run while suspended" }
`)
	k := &suspendingKit{DryRun: NewDryRun(loopkit.Trigger{Kind: loopkit.TriggerManual})}
	err := Run(context.Background(), r, k)
	if err == nil {
		t.Fatal("expected the run to end suspended, not succeed")
	}
	if !loopkit.Suspended(err) {
		t.Fatalf("error lost its identity as it propagated: %v", err)
	}
	if strings.Contains(k.Report(), "after") {
		t.Errorf("a step after the suspend point ran anyway:\n%s", k.Report())
	}
}

// send's upgrade: the old to:-only shape still means exactly what it always
// meant, and channel:/thread: route through SendTo instead without disturbing
// that meaning.
func TestSendRoutesThroughChannelOrKeepsWhatsApp(t *testing.T) {
	r := mustParse(t, `
name: x
on:
  manual: true
steps:
  - send: { to: "+911234567890", text: "old form" }
  - send: { to: "#eng", thread: "t-1", text: "new form" }
`)
	d := NewDryRun(loopkit.Trigger{Kind: loopkit.TriggerManual})
	if err := Run(context.Background(), r, d); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(d.Report(), "SEND WhatsApp to +911234567890: old form") {
		t.Errorf("the old to:-only form changed meaning:\n%s", d.Report())
	}
	if !strings.Contains(d.Report(), "SEND to #eng") || !strings.Contains(d.Report(), "new form") {
		t.Errorf("channel+thread did not route through SendTo:\n%s", d.Report())
	}
}

// propose's upgrade: to_role routes through ProposeTo; leaving it out keeps
// asking the one operator, exactly as before.
func TestProposeRoutesThroughRoleOrKeepsTheOperator(t *testing.T) {
	r := mustParse(t, `
name: x
on:
  manual: true
steps:
  - propose: { title: "old form", action: "a" }
  - propose: { to_role: senior-dev, title: "new form", action: "a" }
`)
	d := NewDryRun(loopkit.Trigger{Kind: loopkit.TriggerManual})
	if err := Run(context.Background(), r, d); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(d.Report(), `ASK YOUR APPROVAL — "old form"`) {
		t.Errorf("the old propose form changed meaning:\n%s", d.Report())
	}
	if !strings.Contains(d.Report(), `holding "senior-dev"`) {
		t.Errorf("to_role did not route through ProposeTo:\n%s", d.Report())
	}
}

// One recipe, every new verb, dry run end to end — the equivalent of
// TestADryRunPerformsNothing for the organisational half of the language.
func TestADryRunCoversEveryOrgVerb(t *testing.T) {
	r := mustParse(t, `
name: org-flow
on:
  manual: true
steps:
  - case.open: { agent: ops-pack, key: "jira:{{ .ticket }}", title: "{{ .summary }}" }
    as: c
  - case.state: { case: "{{ .c.id }}", state: prioritized }
  - case.log:   { case: "{{ .c.id }}", kind: note, payload: "asked for repro" }
  - case.say:   { case: "{{ .c.id }}", channel: "#eng", text: "starting work" }
  - case.history: { case: "{{ .c.id }}" }
    as: hist
  - await:
      event: jira.issue.updated
      match: { key: "{{ .ticket }}", status: Prioritized }
      timeout: 168h
    as: moved
  - sandbox:
      case: "{{ .c.id }}"
      repo: acme/api
      branch: main
      task: "Implement {{ .ticket }}"
      timeout: 45m
    as: build
  - send: { to: "#eng", thread: "{{ .c.thread_ts }}", text: "PR is up" }
  - propose: { to_role: senior-dev, title: "Merge?", summary: "s", action: "a" }
`)
	d := NewDryRun(loopkit.Trigger{Kind: loopkit.TriggerManual, Payload: map[string]any{
		"ticket": "OPS-1", "summary": "fix it",
	}})
	if err := Run(context.Background(), r, d); err != nil {
		t.Fatal(err)
	}
	report := d.Report()
	for _, want := range []string{
		"open the case", "for ops-pack", "move case", "log to case",
		"say in #eng", "read the last", "history",
		"WAIT for", "RUN CODE in a container", "SEND to #eng", "ASK APPROVAL",
	} {
		if !strings.Contains(report, want) {
			t.Errorf("report missing %q:\n%s", want, report)
		}
	}
	if len(d.Actions) != 9 {
		t.Errorf("recorded %d actions, want 9:\n%s", len(d.Actions), report)
	}
}

// The ejected Go for a recipe using every new verb at least PARSES as Go —
// go/format is a cheap syntax check this package's tests can run without
// shelling out to the toolchain; the semantic bugs (variable redeclaration,
// unused locals) that syntax parsing cannot catch were run down by hand with
// a real `go build` against a temp module, which is how the checkpoint-var
// collision and the unused foreach loop variable were actually found.
func TestEjectedGoForEveryNewVerbIsSyntacticallyValid(t *testing.T) {
	r := mustParse(t, `
name: org-flow
on:
  manual: true
steps:
  - case.open: { agent: ops-pack, key: "jira:{{ .ticket }}", title: "{{ .summary }}" }
    as: c
  - case.state: { case: "{{ .c.id }}", state: prioritized }
  - case.log:   { case: "{{ .c.id }}", kind: note, payload: "note" }
  - case.say:   { case: "{{ .c.id }}", channel: "#eng", text: "starting work" }
  - case.history: { case: "{{ .c.id }}" }
    as: hist
  - case.get: { key: "jira:{{ .ticket }}" }
    as: c2
  - await:
      event: jira.issue.updated
      match: { key: "{{ .ticket }}", status: Prioritized }
      timeout: 168h
    as: moved
  - foreach:
      in: ["a", "b"]
      as: t
      steps:
        - ask: "summarise {{ .t }}"
          as: out
  - sandbox:
      case: "{{ .c.id }}"
      repo: acme/api
      branch: main
      task: "Implement {{ .ticket }}"
      timeout: 45m
      env.TOKEN: "sekrit"
    as: build
  - send: { to: "#eng", thread: "{{ .c.thread_ts }}", text: "PR is up" }
  - propose: { to_role: senior-dev, title: "Merge?", summary: "s", action: "a" }
`)
	code := Eject(r, "")
	if _, err := format.Source([]byte(code)); err != nil {
		t.Fatalf("ejected code is not even syntactically valid Go: %v\n\n%s", err, code)
	}
}

// Describe surfaces the new verbs' reach honestly — sandbox runs code, await
// can park for a long time, foreach multiplies whatever it wraps.
func TestDescribeCoversTheOrgVerbs(t *testing.T) {
	r := mustParse(t, `
name: x
on:
  manual: true
steps:
  - sandbox: { repo: o/r, branch: main, task: t }
  - await: { event: e }
  - foreach:
      in: '["a"]'
      as: x
      steps:
        - remember: { fact: "{{ .x }}" }
`)
	out := strings.Join(Describe(r), " | ")
	for _, want := range []string{"RUN CODE", "PARK", "repeat its steps", "WRITE to your long-term memory"} {
		if !strings.Contains(out, want) {
			t.Errorf("Describe missing %q: %s", want, out)
		}
	}
}
