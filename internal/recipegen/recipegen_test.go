package recipegen

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/MelloB1989/karmax/internal/recipes"
)

// A stable, always-valid recipe using only long-established verbs, so these
// tests do not depend on the org verbs' still-incomplete run.go/dryrun.go
// support landing first.
const validRecipe = `name: morning-digest
on:
  schedule: "0 30 8 * * *"
steps:
  - recall: "anything pending"
    as: ctx
  - ask: "Write a short morning briefing using: {{ .ctx }}"
    as: briefing
  - notify:
      title: "Morning briefing"
      body: "{{ .briefing }}"
`

// Missing an 'on:' trigger — a validation error with a known, stable message.
const noTriggerRecipe = `name: x
steps:
  - log: hi
`

type call struct{ system, user string }

// scriptedModel replies from a fixed script, repeating the last reply once
// the script runs out — which is what lets one script stand in for "the
// model never gets it right" without the test caring how many times it is
// asked.
type scriptedModel struct {
	replies []string
	calls   []call
}

func (m *scriptedModel) fn() ModelFunc {
	return func(_ context.Context, sys, user string) (string, error) {
		i := len(m.calls)
		m.calls = append(m.calls, call{sys, user})
		if i >= len(m.replies) {
			i = len(m.replies) - 1
		}
		return m.replies[i], nil
	}
}

func TestGenerateCleanFirstAttempt(t *testing.T) {
	m := &scriptedModel{replies: []string{validRecipe}}
	d, err := Generate(context.Background(), Request{Description: "a morning briefing"}, m.fn())
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if d.Attempts != 1 {
		t.Errorf("Attempts = %d, want 1", d.Attempts)
	}
	if len(m.calls) != 1 {
		t.Fatalf("model called %d times, want 1", len(m.calls))
	}
	if strings.TrimSpace(d.YAML) == "" {
		t.Error("Draft.YAML is empty")
	}
	if !strings.Contains(d.Describe, "ask your agent") {
		t.Errorf("Describe = %q, want it to mention what 'ask' permits", d.Describe)
	}
	if !strings.Contains(d.DryRun, "recall") && !strings.Contains(d.DryRun, "ask the agent") {
		t.Errorf("DryRun = %q, want a recorded rehearsal of the steps", d.DryRun)
	}
}

// The heart of the pipeline: a bad first draft must produce a SECOND prompt
// that actually carries the located error back, and a good second draft must
// converge rather than being treated as one more failure.
func TestGenerateRetriesOnInvalidYAMLAndConverges(t *testing.T) {
	m := &scriptedModel{replies: []string{noTriggerRecipe, validRecipe}}
	d, err := Generate(context.Background(), Request{Description: "a morning briefing"}, m.fn())
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if d.Attempts != 2 {
		t.Errorf("Attempts = %d, want 2", d.Attempts)
	}
	if len(m.calls) != 2 {
		t.Fatalf("model called %d times, want 2", len(m.calls))
	}

	retry := m.calls[1].user
	if !strings.Contains(retry, "nothing triggers this recipe") {
		t.Errorf("second prompt does not carry the located error's message:\n%s", retry)
	}
	if !strings.Contains(retry, "add an 'on:' block") {
		t.Errorf("second prompt does not carry the located error's Fix:\n%s", retry)
	}
	if !strings.Contains(retry, noTriggerRecipe) {
		t.Errorf("second prompt does not show the model its own bad draft:\n%s", retry)
	}
	if strings.TrimSpace(d.YAML) == strings.TrimSpace(noTriggerRecipe) {
		t.Error("Draft.YAML is the invalid first attempt, not the converged one")
	}
}

// A model that never gets it right must not loop forever — it stops at the
// cap and hands back the last, located error plus the best (still invalid)
// draft, so a caller can show the operator something concrete.
func TestGenerateNeverConvergesStopsAtCap(t *testing.T) {
	m := &scriptedModel{replies: []string{noTriggerRecipe}}
	d, err := Generate(context.Background(), Request{Description: "a morning briefing"}, m.fn())
	if err == nil {
		t.Fatal("Generate returned no error for a recipe that never became valid")
	}
	if len(m.calls) != maxAttempts {
		t.Errorf("model called %d times, want the cap of %d", len(m.calls), maxAttempts)
	}
	if d.Attempts != maxAttempts {
		t.Errorf("Attempts = %d, want %d", d.Attempts, maxAttempts)
	}
	var rerr *recipes.Error
	if !errors.As(err, &rerr) {
		t.Fatalf("error is not a located *recipes.Error: %v", err)
	}
	if !strings.Contains(rerr.Message, "nothing triggers this recipe") {
		t.Errorf("error = %q, want the last Parse failure", rerr.Message)
	}
	if strings.TrimSpace(d.YAML) != strings.TrimSpace(noTriggerRecipe) {
		t.Errorf("Draft.YAML = %q, want the best (last) attempt preserved", d.YAML)
	}
}

// The regression test worth having: if this package ever freezes the verb
// list into a string literal instead of asking recipes for it live, a verb
// added to recipe.go later stops reaching the model silently. Comparing the
// prompt against the SAME live source of truth the generator itself queries
// is what would catch that.
func TestVerbListIsDerivedAtRuntimeNotHardcoded(t *testing.T) {
	verbs := recipeVerbs()
	if len(verbs) < 21 {
		t.Fatalf("recipeVerbs() returned %d verbs, want at least 21 — the runtime probe may be broken", len(verbs))
	}
	prompt := systemPrompt()
	for _, v := range verbs {
		if !strings.Contains(prompt, v) {
			t.Errorf("systemPrompt() does not mention verb %q", v)
		}
	}
}

func TestExtractYAMLStripsAFence(t *testing.T) {
	cases := map[string]string{
		"```yaml\nname: x\n```": "name: x",
		"```\nname: x\n```":     "name: x",
		"name: x":               "name: x",
		"  name: x  \n":         "name: x",
	}
	for in, want := range cases {
		if got := extractYAML(in); got != want {
			t.Errorf("extractYAML(%q) = %q, want %q", in, got, want)
		}
	}
}

// A harness prompt that threads a webhook's own payload straight through is
// exactly the case Describe cannot show — it only ever says "this recipe can
// run a harness", never what goes into the call.
func TestInjectionWarningOnUntrustedDataIntoHarness(t *testing.T) {
	const risky = `name: leak-check
on:
  webhook: incoming
steps:
  - harness: "Investigate this: {{ .payload }}"
`
	m := &scriptedModel{replies: []string{risky}}
	d, err := Generate(context.Background(), Request{Description: "investigate a webhook"}, m.fn())
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if len(d.Warnings) == 0 {
		t.Fatal("expected a warning about untrusted data flowing into a harness step")
	}
	joined := strings.Join(d.Warnings, "\n")
	if !strings.Contains(joined, "payload") || !strings.Contains(joined, "harness") {
		t.Errorf("warnings = %q, want it to name the binding and the verb", joined)
	}
}

func TestNoInjectionWarningWithoutUntrustedDataFlow(t *testing.T) {
	const safe = `name: safe-check
on:
  webhook: incoming
steps:
  - harness: "Investigate today's open questions."
`
	m := &scriptedModel{replies: []string{safe}}
	d, err := Generate(context.Background(), Request{Description: "investigate open questions"}, m.fn())
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if len(d.Warnings) != 0 {
		t.Errorf("Warnings = %v, want none — nothing untrusted flows into the harness step", d.Warnings)
	}
}

func TestRefineAmendsThroughTheSameLoop(t *testing.T) {
	const amended = `name: morning-digest
on:
  schedule: "0 30 8 * * *"
steps:
  - recall: "anything pending"
    as: ctx
  - ask: "Write a short morning briefing using: {{ .ctx }}"
    as: briefing
  - notify:
      title: "Morning briefing"
      body: "{{ .briefing }}"
  - when: "{{ .briefing }}"
    send:
      to: "#eng"
      text: "digest failed to produce anything"
`
	m := &scriptedModel{replies: []string{amended}}
	req := RefineRequest{YAML: validRecipe, Instruction: "also notify #eng when it fails"}
	d, err := Refine(context.Background(), req, m.fn())
	if err != nil {
		t.Fatalf("Refine: %v", err)
	}
	if len(m.calls) != 1 {
		t.Fatalf("model called %d times, want 1", len(m.calls))
	}
	sent := m.calls[0].user
	if !strings.Contains(sent, "also notify #eng when it fails") {
		t.Errorf("prompt does not carry the instruction:\n%s", sent)
	}
	if !strings.Contains(sent, "recall: \"anything pending\"") {
		t.Errorf("prompt does not carry the existing recipe:\n%s", sent)
	}
	if d.Attempts != 1 {
		t.Errorf("Attempts = %d, want 1", d.Attempts)
	}
}

func TestGenerateRejectsEmptyDescription(t *testing.T) {
	m := &scriptedModel{replies: []string{validRecipe}}
	if _, err := Generate(context.Background(), Request{}, m.fn()); err == nil {
		t.Error("expected an error for an empty description")
	}
	if len(m.calls) != 0 {
		t.Error("the model must not be called for a request that was refused up front")
	}
}
