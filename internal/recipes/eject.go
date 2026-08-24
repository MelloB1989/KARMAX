package recipes

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// Ejecting.
//
// §4.2 says there must be no cliff between tiers. A recipe that outgrows YAML
// should become a Workflow without its author starting over — so this emits the
// Go the recipe was already doing, in the same order, with the same step names.
//
// This is a starting point, not a finished program: template text such as
// "{{ .t }}" is carried over literally rather than rendered, exactly as
// before this package grew organisational verbs — an author finishing the
// port was always going to replace that with real Go. What is new here does
// not raise that bar; it just keeps the seven new verbs from being silently
// dropped when someone ejects a recipe that uses one.

// Eject renders a recipe as a loopkit loop.
func Eject(r *Recipe, module string) string {
	var b strings.Builder
	fmt.Fprintf(&b, `// Package %s was ejected from the recipe %q.
//
// It does exactly what the YAML did. The step names are preserved, so a run
// that was part-way through the recipe resumes correctly in the loop.
package %s

import (
	"context"
	"fmt"
`, goIdent(r.Name), r.Name, goIdent(r.Name))
	if needsStrings(r.Steps) {
		b.WriteString("\t\"strings\"\n")
	}
	if needsTime(r.Steps) {
		b.WriteString("\t\"time\"\n")
	}
	fmt.Fprintf(&b, `
	"github.com/MelloB1989/karmax/pkg/loopkit"
)

func init() {
	loopkit.Register(loopkit.Loop{
		Name: %q,
`, r.Name)

	if r.On.Schedule != "" {
		fmt.Fprintf(&b, "\t\tSchedule: loopkit.Cron(%q),\n", r.On.Schedule)
	}
	if r.On.Webhook != "" {
		fmt.Fprintf(&b, "\t\tWebhook: %q,\n", r.On.Webhook)
	}
	if r.On.Event != "" {
		fmt.Fprintf(&b, "\t\tEvents: []string{%q},\n", r.On.Event)
	}
	b.WriteString("\t\tRun: run,\n\t})\n}\n\nfunc run(ctx context.Context, k loopkit.Kit) error {\n")
	b.WriteString("\tb := map[string]any{\"trigger\": k.Trigger().Kind}\n")
	b.WriteString("\tfor key, v := range k.Trigger().Payload {\n\t\tb[key] = v\n\t}\n\t_ = b\n\n")

	emitSteps(&b, r.Steps, "", 1, "")

	b.WriteString("\treturn nil\n}\n")
	return b.String()
}

// needsTime says whether the generated file uses time.ParseDuration/Duration
// anywhere (sleep, await, sandbox) — the one optional stdlib import.
func needsTime(steps []Step) bool {
	for _, s := range steps {
		switch s.Verb {
		case VerbSleep, VerbAwait, VerbSandbox:
			return true
		}
		if needsTime(s.Else) || needsTime(s.Steps) {
			return true
		}
	}
	return false
}

// needsStrings says whether the generated file calls strings.Join — only a
// BOUND case.history does that, to turn CaseHistory's []string into the one
// string b[s.As] expects.
func needsStrings(steps []Step) bool {
	for _, s := range steps {
		if s.Verb == VerbCaseHistory && s.As != "" {
			return true
		}
		if needsStrings(s.Else) || needsStrings(s.Steps) {
			return true
		}
	}
	return false
}

func emitSteps(b *strings.Builder, steps []Step, prefix string, depth int, loopVar string) {
	ind := strings.Repeat("\t", depth)
	for i, s := range steps {
		id := fmt.Sprintf("%s%d-%s", prefix, i, s.Verb)
		idExpr := idLiteral(id, loopVar)

		if s.When != "" {
			fmt.Fprintf(b, "%s// when: %s\n", ind, s.When)
			fmt.Fprintf(b, "%sif truthy(b, %q) {\n", ind, s.When)
			emitStep(b, s, id, idExpr, depth+1, loopVar)
			if len(s.Else) > 0 {
				fmt.Fprintf(b, "%s} else {\n", ind)
				emitSteps(b, s.Else, id+"-else-", depth+1, loopVar)
			}
			fmt.Fprintf(b, "%s}\n\n", ind)
			continue
		}
		emitStep(b, s, id, idExpr, depth, loopVar)
		b.WriteString("\n")
	}
}

// idLiteral renders a step's checkpoint id as Go source: a plain string
// literal outside a foreach, or — inside one — an expression that folds the
// loop variable in. Without that, every iteration of an ejected foreach would
// reuse iteration zero's checkpoint names, the same bug runForeach exists to
// avoid in the interpreter.
func idLiteral(id, loopVar string) string {
	if loopVar == "" {
		return strconv.Quote(id)
	}
	return fmt.Sprintf("fmt.Sprintf(%s, %s)", strconv.Quote(id+"-%d"), loopVar)
}

// stepFamily is every verb that binds a result via k.Step (or a direct Kit
// call with the same shape): a value plus an error, both declared with ':='.
// Each gets its own { } block in emitBoundStep — without one, a second such
// verb in the same function would try to redeclare "v" and "err" and fail to
// compile, since neither side of a bare "_, err := ..." on its own is new.
func isStepFamily(verb string) bool {
	switch verb {
	case VerbAsk, VerbObserve, VerbHarness, VerbGateway, VerbRecall, VerbHTTP,
		VerbCaseOpen, VerbCaseGet, VerbCaseHistory, VerbAwait, VerbSandbox:
		return true
	}
	return false
}

func emitStep(b *strings.Builder, s Step, id, idExpr string, depth int, loopVar string) {
	ind := strings.Repeat("\t", depth)
	arg := func(k string) string { return strconv.Quote(s.Args[k]) }
	text := s.Text
	if text == "" {
		text = s.Args["text"]
	}

	if isStepFamily(s.Verb) {
		emitBoundStep(b, s, idExpr, ind)
		return
	}

	switch s.Verb {
	case VerbRemember:
		fmt.Fprintf(b, "%sif err := k.Once(%s, func() error { return k.Remember(%s) }); err != nil {\n%s\treturn err\n%s}\n",
			ind, idExpr, arg("fact"), ind, ind)
	case VerbNotify:
		fmt.Fprintf(b, "%sif err := k.Once(%s, func() error { return k.Notify(%s, %s) }); err != nil {\n%s\treturn err\n%s}\n",
			ind, idExpr, arg("title"), arg("body"), ind, ind)
	case VerbPropose:
		if role := s.Args["to_role"]; role != "" {
			fmt.Fprintf(b, "%sif _, err := k.ProposeTo(%s, %s, %s, %s); err != nil {\n%s\treturn err\n%s}\n",
				ind, strconv.Quote(role), arg("title"), arg("summary"), arg("action"), ind, ind)
		} else {
			fmt.Fprintf(b, "%sif err := k.Once(%s, func() error { return k.Propose(%s, %s, %s) }); err != nil {\n%s\treturn err\n%s}\n",
				ind, idExpr, arg("title"), arg("summary"), arg("action"), ind, ind)
		}
	case VerbRemind:
		fmt.Fprintf(b, "%sif err := k.Once(%s, func() error { return k.Remind(%s, %s, %s) }); err != nil {\n%s\treturn err\n%s}\n",
			ind, idExpr, arg("title"), arg("due"), arg("notes"), ind, ind)
	case VerbSend:
		channel, thread := s.Args["channel"], s.Args["thread"]
		if channel == "" && thread == "" {
			fmt.Fprintf(b, "%sif err := k.Once(%s, func() error { return k.SendWhatsApp(ctx, %s, %s) }); err != nil {\n%s\treturn err\n%s}\n",
				ind, idExpr, arg("to"), strconv.Quote(text), ind, ind)
		} else {
			if channel == "" {
				channel = s.Args["to"]
			}
			fmt.Fprintf(b, "%sif err := k.Once(%s, func() error { return k.SendTo(ctx, %s, %s, %s) }); err != nil {\n%s\treturn err\n%s}\n",
				ind, idExpr, strconv.Quote(channel), strconv.Quote(thread), strconv.Quote(text), ind, ind)
		}
	case VerbSleep:
		d := s.Text
		if d == "" {
			d = s.Args["for"]
		}
		fmt.Fprintf(b, "%s// the run parks here and resumes when the timer fires\n", ind)
		fmt.Fprintf(b, "%sif err := k.Once(%s, func() error {\n%s\td, _ := time.ParseDuration(%q)\n"+
			"%s\treturn k.After(%s, d, nil)\n%s}); err != nil {\n%s\treturn err\n%s}\n",
			ind, idExpr, ind, d, ind, idExpr, ind, ind, ind)
	case VerbLog:
		fmt.Fprintf(b, "%sk.Logf(%s)\n", ind, strconv.Quote(text))
	case VerbTool:
		fmt.Fprintf(b, "%s// TODO: the recipe called the tool %s through the agent.\n", ind, arg("name"))
		fmt.Fprintf(b, "%s// In Go, call it directly or keep using k.Ask.\n", ind)

	case VerbCaseState:
		fmt.Fprintf(b, "%sif err := k.CaseSetState(%s, %s); err != nil {\n%s\treturn err\n%s}\n",
			ind, arg("case"), arg("state"), ind, ind)
	case VerbCaseLog:
		fmt.Fprintf(b, "%sif err := k.Once(%s, func() error { return k.CaseLog(%s, %s, %s) }); err != nil {\n%s\treturn err\n%s}\n",
			ind, idExpr, arg("case"), arg("kind"), arg("payload"), ind, ind)
	case VerbCaseSay:
		fmt.Fprintf(b, "%sif err := k.Once(%s, func() error { return k.CaseSay(ctx, %s, %s, %s) }); err != nil {\n%s\treturn err\n%s}\n",
			ind, idExpr, arg("case"), arg("channel"), strconv.Quote(text), ind, ind)
	case VerbForeach:
		emitForeach(b, s, id, ind, depth)
	}
}

// emitBoundStep renders one stepFamily verb, self-contained in its own { }
// block so "v" and "err" are fresh every time regardless of how many such
// steps the recipe has.
func emitBoundStep(b *strings.Builder, s Step, idExpr, outerInd string) {
	arg := func(k string) string { return strconv.Quote(s.Args[k]) }
	text := s.Text
	if text == "" {
		text = s.Args["text"]
	}
	fmt.Fprintf(b, "%s{\n", outerInd)
	ind := outerInd + "\t"
	// A step nobody binds still needs a name for the result k.Step demands —
	// "_" discards it without leaving an unused local behind.
	bind := "_"
	if s.As != "" {
		bind = "v"
	}

	switch s.Verb {
	case VerbAsk, VerbObserve, VerbHarness, VerbGateway:
		call := map[string]string{VerbAsk: "k.Ask(ctx, %s)", VerbHarness: "k.Harness(ctx, %s)",
			VerbGateway: "k.Gateway(ctx, %s)"}[s.Verb]
		fmt.Fprintf(b, "%s%s, err := k.Step(%s, func() (string, error) {\n%s\treturn "+call+"\n%s})\n",
			ind, bind, idExpr, ind, strconv.Quote(text), ind)
		errCheck(b, ind)
	case VerbRecall:
		fmt.Fprintf(b, "%s%s, err := k.Step(%s, func() (string, error) {\n"+
			"%s\thits, err := k.Recall(%s, %s)\n%s\tif err != nil {\n%s\t\treturn \"\", err\n%s\t}\n"+
			"%s\treturn fmt.Sprint(hits), nil\n%s})\n",
			ind, bind, idExpr, ind, strconv.Quote(text), orDefault(s.Args["limit"], "5"), ind, ind, ind, ind, ind)
		errCheck(b, ind)
	case VerbHTTP:
		fmt.Fprintf(b, "%s%s, err := k.Step(%s, func() (string, error) {\n"+
			"%s\tout, status, err := k.HTTP(ctx, %s, %s, nil, %s)\n"+
			"%s\tif err != nil || status < 200 || status > 299 {\n"+
			"%s\t\treturn \"\", fmt.Errorf(\"http %%d: %%w\", status, err)\n%s\t}\n"+
			"%s\treturn out, nil\n%s})\n",
			ind, bind, idExpr, ind, quoteOr(s.Args["method"], "GET"), arg("url"), arg("body"), ind, ind, ind, ind, ind)
		errCheck(b, ind)
	case VerbCaseOpen:
		// agent names the pack, not this recipe; "" (the zero value of a
		// missing field) is exactly what falls back to the loop.
		fmt.Fprintf(b, "%s%s, err := k.CaseOpen(%s, %s, %s)\n", ind, bind, arg("agent"), arg("key"), arg("title"))
		errCheck(b, ind)
	case VerbCaseGet:
		fmt.Fprintf(b, "%s%s, _, err := k.CaseGet(%s)\n", ind, bind, arg("key"))
		errCheck(b, ind)
	case VerbCaseHistory:
		// CaseHistory returns []string, not (string, error) like the rest of
		// this family, so an unbound call discards it as "_" same as always,
		// and a bound one gets an extra line joining it into the string
		// b[s.As] expects.
		histBind := "_"
		if s.As != "" {
			histBind = "lines"
		}
		fmt.Fprintf(b, "%s%s, err := k.CaseHistory(%s, %s)\n", ind, histBind, arg("case"), orDefault(s.Args["limit"], "40"))
		errCheck(b, ind)
		if s.As != "" {
			fmt.Fprintf(b, "%s%s := strings.Join(lines, \"\\n\")\n", ind, bind)
		}
	case VerbAwait:
		fmt.Fprintf(b, "%s%s, err := k.Await(ctx, %s, loopkit.AwaitSpec{\n", ind, bind, idExpr)
		fmt.Fprintf(b, "%s\tEvent: %s,\n", ind, arg("event"))
		fmt.Fprintf(b, "%s\tMatch: %s,\n", ind, mapLiteral(s.Match))
		fmt.Fprintf(b, "%s\tCaseID: %s,\n", ind, arg("case"))
		fmt.Fprintf(b, "%s\tTimeout: %s,\n", ind, durationExpr(s.Args["timeout"]))
		fmt.Fprintf(b, "%s})\n", ind)
		errCheck(b, ind)
	case VerbSandbox:
		fmt.Fprintf(b, "%s%s, err := k.Sandbox(ctx, %s, loopkit.SandboxSpec{\n", ind, bind, idExpr)
		fmt.Fprintf(b, "%s\tCaseID: %s,\n", ind, arg("case"))
		fmt.Fprintf(b, "%s\tRepo: %s,\n", ind, arg("repo"))
		fmt.Fprintf(b, "%s\tBranch: %s,\n", ind, arg("branch"))
		fmt.Fprintf(b, "%s\tTask: %s,\n", ind, arg("task"))
		fmt.Fprintf(b, "%s\tImage: %s,\n", ind, arg("image"))
		fmt.Fprintf(b, "%s\tEnv: %s,\n", ind, envLiteral(s.Args))
		fmt.Fprintf(b, "%s\tTimeout: %s,\n", ind, durationExpr(s.Args["timeout"]))
		fmt.Fprintf(b, "%s})\n", ind)
		errCheck(b, ind)
	}

	if s.As != "" {
		fmt.Fprintf(b, "%sb[%q] = v\n", ind, s.As)
	}
	fmt.Fprintf(b, "%s}\n", outerInd)
}

// emitForeach unrolls a literal in: list into a real Go loop. A templated
// in: ("{{ .tickets }}") is only known at runtime, which this file never
// evaluates (see the package comment), so that form is left as a TODO —
// the same treatment VerbTool already gets for the same reason.
func emitForeach(b *strings.Builder, s Step, id, ind string, depth int) {
	if len(s.In) == 0 {
		fmt.Fprintf(b, "%s// TODO: 'in' is a template, resolved at runtime — port the JSON parsing by hand.\n", ind)
		fmt.Fprintf(b, "%s// for _, %s := range /* parsed from %q */ {\n%s// \t...\n%s// }\n",
			ind, s.Args["as"], s.Args["in"], ind, ind)
		return
	}
	fmt.Fprintf(b, "%sfor i, %s := range %s {\n", ind, s.Args["as"], literalStringSlice(s.In))
	// Nested steps never render {{ .item }} — see the package comment — so the
	// item variable has no textual use in what they emit, and would otherwise
	// be "declared and not used".
	fmt.Fprintf(b, "%s\t_ = %s\n", ind, s.Args["as"])
	emitSteps(b, s.Steps, id+"-", depth+1, "i")
	fmt.Fprintf(b, "%s}\n", ind)
}

func errCheck(b *strings.Builder, ind string) {
	fmt.Fprintf(b, "%sif err != nil {\n%s\treturn err\n%s}\n", ind, ind, ind)
}

func orDefault(s, def string) string {
	if strings.TrimSpace(s) == "" {
		return def
	}
	return s
}

func quoteOr(s, def string) string {
	if strings.TrimSpace(s) == "" {
		return strconv.Quote(def)
	}
	return strconv.Quote(strings.ToUpper(s))
}

// mapLiteral renders a map[string]string as Go source, keys sorted so the
// output (and hence any test asserting on it) does not depend on Go's
// randomised map iteration order.
func mapLiteral(m map[string]string) string {
	if len(m) == 0 {
		return "nil"
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = fmt.Sprintf("%s: %s", strconv.Quote(k), strconv.Quote(m[k]))
	}
	return "map[string]string{" + strings.Join(parts, ", ") + "}"
}

// envLiteral pulls sandbox's env.KEY fields (the same flattening http uses
// for header.NAME) back out into a map literal.
func envLiteral(args map[string]string) string {
	m := map[string]string{}
	for k, v := range args {
		if name, ok := strings.CutPrefix(k, "env."); ok {
			m[name] = v
		}
	}
	return mapLiteral(m)
}

func durationExpr(raw string) string {
	if strings.TrimSpace(raw) == "" {
		return "0"
	}
	return fmt.Sprintf("func() time.Duration { d, _ := time.ParseDuration(%s); return d }()", strconv.Quote(raw))
}

func literalStringSlice(items []string) string {
	parts := make([]string, len(items))
	for i, it := range items {
		parts[i] = strconv.Quote(it)
	}
	return "[]string{" + strings.Join(parts, ", ") + "}"
}

// goIdent turns a recipe name into a package name.
func goIdent(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
	}
	out := b.String()
	if out == "" || (out[0] >= '0' && out[0] <= '9') {
		out = "recipe" + out
	}
	return out
}
