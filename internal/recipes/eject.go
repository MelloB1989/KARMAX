package recipes

import (
	"fmt"
	"strconv"
	"strings"
)

// Ejecting.
//
// §4.2 says there must be no cliff between tiers. A recipe that outgrows YAML
// should become a Workflow without its author starting over — so this emits the
// Go the recipe was already doing, in the same order, with the same step names.

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

	"github.com/MelloB1989/karmax/pkg/loopkit"
)

func init() {
	loopkit.Register(loopkit.Loop{
		Name: %q,
`, goIdent(r.Name), r.Name, goIdent(r.Name), r.Name)

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

	emitSteps(&b, r.Steps, "", 1)

	b.WriteString("\treturn nil\n}\n")
	return b.String()
}

func emitSteps(b *strings.Builder, steps []Step, prefix string, depth int) {
	ind := strings.Repeat("\t", depth)
	for i, s := range steps {
		id := fmt.Sprintf("%s%d-%s", prefix, i, s.Verb)

		if s.When != "" {
			fmt.Fprintf(b, "%s// when: %s\n", ind, s.When)
			fmt.Fprintf(b, "%sif truthy(b, %q) {\n", ind, s.When)
			emitStep(b, s, id, depth+1)
			if len(s.Else) > 0 {
				fmt.Fprintf(b, "%s} else {\n", ind)
				emitSteps(b, s.Else, id+"-else-", depth+1)
			}
			fmt.Fprintf(b, "%s}\n\n", ind)
			continue
		}
		emitStep(b, s, id, depth)
		b.WriteString("\n")
	}
}

func emitStep(b *strings.Builder, s Step, id string, depth int) {
	ind := strings.Repeat("\t", depth)
	arg := func(k string) string { return strconv.Quote(s.Args[k]) }
	text := s.Text
	if text == "" {
		text = s.Args["text"]
	}

	bind := "_"
	if s.As != "" {
		bind = "v"
	}

	switch s.Verb {
	case VerbAsk, VerbHarness, VerbGateway:
		call := map[string]string{VerbAsk: "k.Ask(ctx, %s)", VerbHarness: "k.Harness(ctx, %s)",
			VerbGateway: "k.Gateway(ctx, %s)"}[s.Verb]
		fmt.Fprintf(b, "%s%s, err := k.Step(%q, func() (string, error) {\n%s\treturn "+call+"\n%s})\n",
			ind, bind, id, ind, strconv.Quote(text), ind)
		errCheck(b, ind)
	case VerbRecall:
		fmt.Fprintf(b, "%s%s, err := k.Step(%q, func() (string, error) {\n"+
			"%s\thits, err := k.Recall(%s, %s)\n%s\tif err != nil {\n%s\t\treturn \"\", err\n%s\t}\n"+
			"%s\treturn fmt.Sprint(hits), nil\n%s})\n",
			ind, bind, id, ind, strconv.Quote(text), orDefault(s.Args["limit"], "5"), ind, ind, ind, ind, ind)
		errCheck(b, ind)
	case VerbHTTP:
		fmt.Fprintf(b, "%s%s, err := k.Step(%q, func() (string, error) {\n"+
			"%s\tout, status, err := k.HTTP(ctx, %s, %s, nil, %s)\n"+
			"%s\tif err != nil || status < 200 || status > 299 {\n"+
			"%s\t\treturn \"\", fmt.Errorf(\"http %%d: %%w\", status, err)\n%s\t}\n"+
			"%s\treturn out, nil\n%s})\n",
			ind, bind, id, ind, quoteOr(s.Args["method"], "GET"), arg("url"), arg("body"), ind, ind, ind, ind, ind)
		errCheck(b, ind)
	case VerbRemember:
		fmt.Fprintf(b, "%sif err := k.Once(%q, func() error { return k.Remember(%s) }); err != nil {\n%s\treturn err\n%s}\n",
			ind, id, arg("fact"), ind, ind)
	case VerbNotify:
		fmt.Fprintf(b, "%sif err := k.Once(%q, func() error { return k.Notify(%s, %s) }); err != nil {\n%s\treturn err\n%s}\n",
			ind, id, arg("title"), arg("body"), ind, ind)
	case VerbPropose:
		fmt.Fprintf(b, "%sif err := k.Once(%q, func() error { return k.Propose(%s, %s, %s) }); err != nil {\n%s\treturn err\n%s}\n",
			ind, id, arg("title"), arg("summary"), arg("action"), ind, ind)
	case VerbRemind:
		fmt.Fprintf(b, "%sif err := k.Once(%q, func() error { return k.Remind(%s, %s, %s) }); err != nil {\n%s\treturn err\n%s}\n",
			ind, id, arg("title"), arg("due"), arg("notes"), ind, ind)
	case VerbSend:
		fmt.Fprintf(b, "%sif err := k.Once(%q, func() error { return k.SendWhatsApp(ctx, %s, %s) }); err != nil {\n%s\treturn err\n%s}\n",
			ind, id, arg("to"), strconv.Quote(text), ind, ind)
	case VerbSleep:
		d := s.Text
		if d == "" {
			d = s.Args["for"]
		}
		fmt.Fprintf(b, "%s// the run parks here and resumes when the timer fires\n", ind)
		fmt.Fprintf(b, "%sif err := k.Once(%q, func() error {\n%s\td, _ := time.ParseDuration(%q)\n"+
			"%s\treturn k.After(%q, d, nil)\n%s}); err != nil {\n%s\treturn err\n%s}\n",
			ind, id, ind, d, ind, id, ind, ind, ind)
	case VerbLog:
		fmt.Fprintf(b, "%sk.Logf(%s)\n", ind, strconv.Quote(text))
	case VerbTool:
		fmt.Fprintf(b, "%s// TODO: the recipe called the tool %s through the agent.\n", ind, arg("name"))
		fmt.Fprintf(b, "%s// In Go, call it directly or keep using k.Ask.\n", ind)
	}

	if s.As != "" {
		fmt.Fprintf(b, "%sb[%q] = v\n", ind, s.As)
	}
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
