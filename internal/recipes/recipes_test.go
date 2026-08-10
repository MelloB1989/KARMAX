package recipes

import (
	"context"
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
		`loopkit.Cron("0 9 * * *")`,
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
