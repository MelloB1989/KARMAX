package recipes

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"text/template"
	"time"

	"github.com/MelloB1989/karmax/pkg/loopkit"
)

// Running a recipe.
//
// Each step is a k.Step checkpoint keyed by its position and verb, so a recipe
// that fails at step four does not resend the message from step two when it is
// retried. That is the whole payoff of building checkpointing first.

// Bindings are the values 'as' has stored, available to later templates.
type Bindings map[string]any

// Run executes a recipe against a Kit.
func Run(ctx context.Context, r *Recipe, k loopkit.Kit) error {
	b := Bindings{
		"trigger": k.Trigger().Kind,
		"payload": k.Trigger().Payload,
		"now":     time.Now().Format(time.RFC3339),
		"today":   time.Now().Format("2006-01-02"),
	}
	for key, v := range k.Trigger().Payload {
		b[key] = v
	}
	return runSteps(ctx, r, k, b, r.Steps, "")
}

func runSteps(ctx context.Context, r *Recipe, k loopkit.Kit, b Bindings, steps []Step, prefix string) error {
	for i, s := range steps {
		id := fmt.Sprintf("%s%d-%s", prefix, i, s.Verb)

		if s.When != "" {
			ok, err := truthy(s.When, b)
			if err != nil {
				return fmt.Errorf("%s:%d: %w", r.Path, s.Line, err)
			}
			if !ok {
				if len(s.Else) > 0 {
					if err := runSteps(ctx, r, k, b, s.Else, id+"-else-"); err != nil {
						return err
					}
				}
				continue
			}
		}

		out, err := runStep(ctx, r, k, b, s, id)
		if err != nil {
			return fmt.Errorf("%s:%d: %s: %w", r.Path, s.Line, s.Verb, err)
		}
		if s.As != "" {
			b[s.As] = out
		}
	}
	return nil
}

func runStep(ctx context.Context, r *Recipe, k loopkit.Kit, b Bindings, s Step, id string) (any, error) {
	arg := func(key string) (string, error) { return render(s.Args[key], b) }
	text := func() (string, error) {
		if s.Text != "" {
			return render(s.Text, b)
		}
		return render(s.Args["text"], b)
	}

	switch s.Verb {
	case VerbAsk:
		p, err := text()
		if err != nil {
			return nil, err
		}
		return k.Step(id, func() (string, error) { return k.Ask(ctx, p) })

	case VerbObserve:
		// A pass that reads and remembers, and cannot send.
		//
		// hot-sync was an `ask` told in capitals that it does not speak. It
		// spoke: it answered a question in the operator's own chat ninety
		// minutes after that question had already been answered, so the
		// operator saw KARMAX reply twice to one message. Instructions are not
		// a boundary. This verb removes the tools instead of forbidding them.
		p, err := text()
		if err != nil {
			return nil, err
		}
		return k.Step(id, func() (string, error) { return k.Observe(ctx, p) })

	case VerbHarness:
		p, err := text()
		if err != nil {
			return nil, err
		}
		return k.Step(id, func() (string, error) { return k.Harness(ctx, p) })

	case VerbGateway:
		p, err := text()
		if err != nil {
			return nil, err
		}
		return k.Step(id, func() (string, error) { return k.Gateway(ctx, p) })

	case VerbRecall:
		q, err := text()
		if err != nil {
			return nil, err
		}
		limit := intArg(s.Args["limit"], 5)
		return k.Step(id, func() (string, error) {
			hits, err := k.Recall(q, limit)
			if err != nil {
				return "", err
			}
			return strings.Join(hits, "\n"), nil
		})

	case VerbRemember:
		fact, err := arg("fact")
		if err != nil {
			return nil, err
		}
		return nil, k.Once(id, func() error { return k.Remember(fact) })

	case VerbNotify:
		title, err := arg("title")
		if err != nil {
			return nil, err
		}
		body, err := arg("body")
		if err != nil {
			return nil, err
		}
		return nil, k.Once(id, func() error { return k.Notify(title, body) })

	case VerbPropose:
		title, err := arg("title")
		if err != nil {
			return nil, err
		}
		summary, err := arg("summary")
		if err != nil {
			return nil, err
		}
		action, err := arg("action")
		if err != nil {
			return nil, err
		}
		return nil, k.Once(id, func() error { return k.Propose(title, summary, action) })

	case VerbRemind:
		title, err := arg("title")
		if err != nil {
			return nil, err
		}
		due, err := arg("due")
		if err != nil {
			return nil, err
		}
		notes, err := arg("notes")
		if err != nil {
			return nil, err
		}
		return nil, k.Once(id, func() error { return k.Remind(title, due, notes) })

	case VerbSend:
		to, err := arg("to")
		if err != nil {
			return nil, err
		}
		body, err := text()
		if err != nil {
			return nil, err
		}
		return nil, k.Once(id, func() error { return k.SendWhatsApp(ctx, to, body) })

	case VerbHTTP:
		url, err := arg("url")
		if err != nil {
			return nil, err
		}
		method := strings.ToUpper(s.Args["method"])
		if method == "" {
			method = "GET"
		}
		body, err := arg("body")
		if err != nil {
			return nil, err
		}
		headers := map[string]string{}
		for key, v := range s.Args {
			if h, ok := strings.CutPrefix(key, "header."); ok {
				rendered, err := render(v, b)
				if err != nil {
					return nil, err
				}
				headers[h] = rendered
			}
		}
		return k.Step(id, func() (string, error) {
			out, status, err := k.HTTP(ctx, method, url, headers, body)
			if err != nil {
				return "", err
			}
			if status < 200 || status > 299 {
				return "", fmt.Errorf("%s %s answered %d", method, url, status)
			}
			return out, nil
		})

	case VerbTool:
		name, err := arg("name")
		if err != nil {
			return nil, err
		}
		input := map[string]any{}
		for key, v := range s.Args {
			if key == "name" {
				continue
			}
			rendered, err := render(v, b)
			if err != nil {
				return nil, err
			}
			input[key] = rendered
		}
		raw, _ := json.Marshal(input)
		return k.Step(id, func() (string, error) {
			// Routed through the agent, so a recipe reaches exactly the tools
			// the agent has and the same permissions apply.
			return k.Ask(ctx, fmt.Sprintf(
				"Call the tool %q with exactly this input and reply with only its result:\n%s",
				name, raw))
		})

	case VerbSleep:
		d, err := duration(s, b)
		if err != nil {
			return nil, err
		}
		// Short waits happen in-process; long ones park the run on a durable
		// timer, which is what makes "check back Thursday" a recipe primitive.
		if d < time.Minute {
			return nil, k.Sleep(ctx, d)
		}
		return nil, k.Once(id, func() error {
			return k.After("recipe-"+r.Name+"-"+id, d, map[string]any{
				"recipe": r.Name, "resumed_from": id,
			})
		})

	case VerbLog:
		msg, err := text()
		if err != nil {
			return nil, err
		}
		k.Logf("%s", msg)
		return nil, nil
	}
	return nil, fmt.Errorf("unknown verb %q", s.Verb)
}

func duration(s Step, b Bindings) (time.Duration, error) {
	raw := s.Text
	if raw == "" {
		raw = s.Args["for"]
	}
	rendered, err := render(raw, b)
	if err != nil {
		return 0, err
	}
	d, err := time.ParseDuration(strings.TrimSpace(rendered))
	if err != nil {
		return 0, fmt.Errorf("%q is not a duration (try 30m, 6h, 72h)", rendered)
	}
	return d, nil
}

// render expands {{ .binding }} references.
func render(s string, b Bindings) (string, error) {
	if !strings.Contains(s, "{{") {
		return s, nil
	}
	t, err := template.New("s").Option("missingkey=zero").Parse(s)
	if err != nil {
		return "", fmt.Errorf("bad template %q: %w", s, err)
	}
	var out strings.Builder
	if err := t.Execute(&out, b); err != nil {
		return "", fmt.Errorf("could not fill in %q: %w", s, err)
	}
	return out.String(), nil
}

// truthy evaluates a 'when'. Deliberately not an expression language: a
// condition someone has to learn a grammar for belongs in a Workflow.
func truthy(expr string, b Bindings) (bool, error) {
	rendered, err := render(expr, b)
	if err != nil {
		return false, err
	}
	v := strings.TrimSpace(strings.ToLower(rendered))
	switch v {
	case "", "false", "0", "no", "off", "<no value>", "null", "nil", "[]", "{}":
		return false, nil
	case "true", "1", "yes", "on":
		return true, nil
	}
	// Anything else non-empty counts as present, which is what "when: {{ .x }}"
	// is nearly always asking.
	return true, nil
}

func intArg(s string, def int) int {
	if n, err := strconv.Atoi(strings.TrimSpace(s)); err == nil && n > 0 {
		return n
	}
	return def
}
