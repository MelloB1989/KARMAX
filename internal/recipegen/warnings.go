package recipegen

import (
	"fmt"
	"regexp"

	"github.com/MelloB1989/karmax/internal/recipes"
)

// Prompt-injection surfacing.
//
// The admin's description is trusted; what a recipe reads at RUN time is not
// — a webhook body, an event payload, a fetched page. recipes.Describe only
// ever says "this recipe can run a harness" or "call the tool X": true, but
// it does not say WHAT goes into that call, so a recipe that pipes a webhook
// body straight into a harness prompt looks the same to an approver as one
// that doesn't. This does not sandbox anything — it just refuses to let that
// difference stay invisible.

var bindingRefRe = regexp.MustCompile(`\{\{\s*\.(\w+)`)

// injectionWarnings flags a tool/harness step whose prompt threads a binding
// that came from outside KARMAX straight through.
func injectionWarnings(r *recipes.Recipe) []string {
	if r == nil {
		return nil
	}
	ext := externalBindings(r)
	if len(ext) == 0 {
		return nil
	}
	var out []string
	var walk func([]recipes.Step)
	walk = func(steps []recipes.Step) {
		for _, s := range steps {
			if s.Verb == recipes.VerbTool || s.Verb == recipes.VerbHarness {
				if name, ok := referencesAny(s, ext); ok {
					out = append(out, fmt.Sprintf(
						"line %d: %s's prompt includes {{ .%s }}, which comes from outside KARMAX — "+
							"that data flow is not visible in the plain-English disclosure, so review it before enabling",
						s.Line, s.Verb, name))
				}
			}
			walk(s.Else)
			walk(s.Steps) // a foreach body is where this would otherwise hide
		}
	}
	walk(r.Steps)
	return out
}

// externalBindings names bindings whose content this recipe did not itself
// produce: the trigger's own payload, for a recipe an outsider can fire
// (webhook or event — a schedule or a manual run has no such author), plus
// whatever an 'http' fetch or an 'await'ed event bound with 'as'.
func externalBindings(r *recipes.Recipe) map[string]bool {
	ext := map[string]bool{}
	if r.On.Webhook != "" || r.On.Event != "" {
		ext["trigger"] = true
		ext["payload"] = true
	}
	var walk func([]recipes.Step)
	walk = func(steps []recipes.Step) {
		for _, s := range steps {
			if s.As != "" && (s.Verb == recipes.VerbHTTP || s.Verb == recipes.VerbAwait) {
				ext[s.As] = true
			}
			walk(s.Else)
			walk(s.Steps) // a foreach body is where this would otherwise hide
		}
	}
	walk(r.Steps)
	return ext
}

// referencesAny reports the first binding in names that s's text or fields
// reference via {{ .name }}.
func referencesAny(s recipes.Step, names map[string]bool) (string, bool) {
	texts := make([]string, 0, len(s.Args)+1)
	texts = append(texts, s.Text)
	for _, v := range s.Args {
		texts = append(texts, v)
	}
	for _, t := range texts {
		for _, m := range bindingRefRe.FindAllStringSubmatch(t, -1) {
			if names[m[1]] {
				return m[1], true
			}
		}
	}
	return "", false
}
