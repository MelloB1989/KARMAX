// Package recipegen turns a plain-English description into a recipe.
//
// The generation target is a recipe, not code: a ~21-verb DSL is a thing a
// model can be made to emit correctly, and everything needed to make a
// generated workflow SAFE already lives in internal/recipes — Parse to catch
// a bad draft, dryrun to rehearse it, Describe to say in plain English what
// running it would permit. This package wires a model to that pipeline and
// stops there. It never writes a recipe to disk and never enables one — an
// admin approves a Draft; something else installs it. That boundary is the
// feature's entire safety story, so it does not blur here.
package recipegen

import (
	"context"
	"fmt"
	"strings"

	"github.com/MelloB1989/karmax/internal/recipes"
	"github.com/MelloB1989/karmax/pkg/loopkit"
)

// maxAttempts bounds the draft/validate loop. A model that cannot produce
// valid YAML on the third try is not going to on the fourth either, and an
// honest failure — with the located error attached — beats a caller waiting
// on a loop with no exit.
const maxAttempts = 3

// ModelFunc is how Generate and Refine reach a model: system prompt in, raw
// reply out. The runtime wires one against karmahelper; tests wire in a fake.
// Keeping the model out of this package's own code is what makes the retry
// loop testable without a network, and what stops this package from picking
// a model client on its own.
type ModelFunc func(ctx context.Context, systemPrompt, userPrompt string) (string, error)

// Request describes the workflow to build, in the admin's own words. The
// description is trusted — it comes from a logged-in admin — which is why
// this package will act on it directly; what a generated recipe reads at
// RUN time is a different, untrusted story or the injectionWarnings below.
type Request struct {
	Description string
	Agent       string
	// Context is free-form detail the caller already has — a repo slug, a
	// channel name, a ticket key — folded into the prompt so the model is
	// not left inventing placeholders for things it was already told.
	Context map[string]string
}

// RefineRequest amends an existing recipe rather than starting from nothing.
type RefineRequest struct {
	YAML        string // the recipe as it stands today
	Instruction string // the change, in English — "also notify #eng when it fails"
	Agent       string
	Context     map[string]string
}

// Draft is a generated recipe: the YAML, what it would do (DryRun), what
// enabling it would permit (Describe), and anything worth a second look
// before it is approved. Attempts counts how many drafts it took to reach
// something that parses — 1 on a clean first try.
type Draft struct {
	YAML     string
	Describe string
	DryRun   string
	Warnings []string
	Attempts int
}

// Generate turns req into a Draft. On success the returned YAML is exactly
// what Parse, dryrun, and Describe were run against — never an earlier,
// broken attempt.
func Generate(ctx context.Context, req Request, model ModelFunc) (Draft, error) {
	if strings.TrimSpace(req.Description) == "" {
		return Draft{}, fmt.Errorf("recipegen: empty description")
	}
	return converge(ctx, systemPrompt(), draftPrompt(req), model)
}

// Refine amends req.YAML per req.Instruction through the same
// validate/rehearse/disclose loop Generate uses, so an edit is held to the
// same bar as a fresh draft rather than trusted because it started valid.
func Refine(ctx context.Context, req RefineRequest, model ModelFunc) (Draft, error) {
	if strings.TrimSpace(req.YAML) == "" {
		return Draft{}, fmt.Errorf("recipegen: no recipe to refine")
	}
	if strings.TrimSpace(req.Instruction) == "" {
		return Draft{}, fmt.Errorf("recipegen: empty instruction")
	}
	return converge(ctx, systemPrompt(), refinePrompt(req), model)
}

// converge runs the draft/validate loop. A failed Parse hands the model back
// its own located error — path:line, the message, and the Fix — which is a
// far shorter path to a correct next draft than re-explaining the schema.
func converge(ctx context.Context, sys, user string, model ModelFunc) (Draft, error) {
	var lastYAML string
	var lastErr error

	for attempt := 1; attempt <= maxAttempts; attempt++ {
		raw, err := model(ctx, sys, user)
		if err != nil {
			return Draft{YAML: lastYAML, Attempts: attempt},
				fmt.Errorf("recipegen: model call failed on attempt %d: %w", attempt, err)
		}
		lastYAML = extractYAML(raw)

		r, perr := recipes.Parse("generated.yaml", []byte(lastYAML))
		if perr == nil {
			return build(ctx, r, lastYAML, attempt), nil
		}
		lastErr = perr
		user = retryPrompt(lastYAML, perr)
	}

	return Draft{
		YAML:     lastYAML,
		Attempts: maxAttempts,
		Warnings: []string{fmt.Sprintf(
			"did not converge on a valid recipe after %d attempts — see the returned error for the last problem", maxAttempts)},
	}, lastErr
}

// build runs the rehearse and disclose stages against a recipe that has
// already parsed. Nothing here fails the Draft: a dry run's own error is
// part of what the operator is shown, not a reason to withhold the rest.
func build(ctx context.Context, r *recipes.Recipe, y string, attempt int) Draft {
	return Draft{
		YAML:     y,
		Attempts: attempt,
		DryRun:   rehearse(ctx, r),
		Describe: strings.Join(recipes.Describe(r), "\n"),
		Warnings: injectionWarnings(r),
	}
}

// rehearse runs the recipe against dryrun's fake Kit, at a fixed clock, so a
// generated recipe never touches a real WhatsApp account, mailbox, or
// container before an operator has approved it.
func rehearse(ctx context.Context, r *recipes.Recipe) string {
	kind := loopkit.TriggerManual
	switch {
	case r.On.Schedule != "":
		kind = loopkit.TriggerSchedule
	case r.On.Event != "":
		kind = loopkit.TriggerEvent
	case r.On.Webhook != "":
		kind = loopkit.TriggerWebhook
	}
	dry := recipes.NewDryRun(loopkit.Trigger{Kind: kind, Payload: map[string]any{}})
	if err := recipes.Run(ctx, r, dry); err != nil {
		return dry.Report() + fmt.Sprintf("\n(then it would fail: %s)", err)
	}
	return dry.Report()
}

// extractYAML strips a ```yaml fence if the model wrapped its answer in one
// despite being asked for YAML only — cheap insurance against the one reply
// shape that would otherwise fail Parse for a reason that has nothing to do
// with the recipe itself.
func extractYAML(raw string) string {
	s := strings.TrimSpace(raw)
	if !strings.HasPrefix(s, "```") {
		return s
	}
	lines := strings.Split(s, "\n")
	if len(lines) < 2 {
		return s
	}
	lines = lines[1:] // drop the opening fence, with its optional "yaml" tag
	if len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "```" {
		lines = lines[:len(lines)-1]
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}
