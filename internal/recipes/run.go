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
		role, err := arg("to_role")
		if err != nil {
			return nil, err
		}
		if role != "" {
			// The proposal id ProposeTo returns is not bound to anything —
			// this dialect never learned to bind it, same as plain propose.
			return nil, k.Once(id, func() error {
				_, err := k.ProposeTo(role, title, summary, action)
				return err
			})
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
		channel, err := arg("channel")
		if err != nil {
			return nil, err
		}
		thread, err := arg("thread")
		if err != nil {
			return nil, err
		}
		if channel == "" && thread == "" {
			// The old shape, unchanged: 'to' is a WhatsApp target. Existing
			// recipes must keep meaning exactly what they always meant.
			return nil, k.Once(id, func() error { return k.SendWhatsApp(ctx, to, body) })
		}
		// 'channel:' or 'thread:' present — this send is aimed at a shared
		// place, not a person. 'to' doubles as the channel when 'channel:'
		// itself is left out, since "#eng" already IS the channel.
		if channel == "" {
			channel = to
		}
		return nil, k.Once(id, func() error { return k.SendTo(ctx, channel, thread, body) })

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

	case VerbCaseOpen:
		key, err := arg("key")
		if err != nil {
			return nil, err
		}
		title, err := arg("title")
		if err != nil {
			return nil, err
		}
		// agent names the PACK, so every workflow in it opens cases under one
		// identity. Absent, the loop's own name stands in.
		agent := s.Args["agent"]
		if agent != "" {
			if agent, err = render(agent, b); err != nil {
				return nil, err
			}
		}
		// Upsert on key, so no checkpoint is needed: a retry rejoins the same
		// case rather than forking a second one.
		c, err := k.CaseOpen(agent, key, title)
		if err != nil {
			return nil, err
		}
		return caseBinding(c), nil

	case VerbCaseGet:
		key, err := arg("key")
		if err != nil {
			return nil, err
		}
		// Not found comes back as the zero Case, which caseBinding turns into
		// a binding of empty strings — false in a 'when:', not an error.
		c, _, err := k.CaseGet(key)
		if err != nil {
			return nil, err
		}
		return caseBinding(c), nil

	case VerbCaseHistory:
		caseID, err := arg("case")
		if err != nil {
			return nil, err
		}
		lines, err := k.CaseHistory(caseID, intArg(s.Args["limit"], 40))
		if err != nil {
			return nil, err
		}
		return strings.Join(lines, "\n"), nil

	case VerbCaseSay:
		caseID, err := arg("case")
		if err != nil {
			return nil, err
		}
		channel, err := arg("channel")
		if err != nil {
			return nil, err
		}
		body, err := text()
		if err != nil {
			return nil, err
		}
		// Checkpointed under the STEP's id, not the case's — reusing the case
		// id here would make every case.say on the same case share one
		// checkpoint, so a second one in the same run would look "already
		// done" and never actually speak.
		return nil, k.Once(id, func() error { return k.CaseSay(ctx, caseID, channel, body) })

	case VerbCaseState:
		caseID, err := arg("case")
		if err != nil {
			return nil, err
		}
		state, err := arg("state")
		if err != nil {
			return nil, err
		}
		// Setting a case to the state it is already in is a no-op worth
		// repeating, so this needs no k.Once either.
		return nil, k.CaseSetState(caseID, state)

	case VerbCaseLog:
		caseID, err := arg("case")
		if err != nil {
			return nil, err
		}
		kind, err := arg("kind")
		if err != nil {
			return nil, err
		}
		payload, err := arg("payload")
		if err != nil {
			return nil, err
		}
		// Unlike the other case verbs this one is NOT idempotent — a retry
		// would append the note twice — so it earns the checkpoint.
		return nil, k.Once(id, func() error { return k.CaseLog(caseID, kind, payload) })

	case VerbAwait:
		spec, err := awaitSpec(s, b)
		if err != nil {
			return nil, err
		}
		// k.Await checkpoints itself under id: a resumed run calls it again
		// and gets the stored result instead of arming a second waiter. When
		// it parks, it returns loopkit.ErrSuspended — passed straight through,
		// not folded into "the step failed", so runSteps and Run propagate it
		// unchanged for the runtime to tell apart from a real failure.
		return k.Await(ctx, id, spec)

	case VerbForeach:
		return nil, runForeach(ctx, r, k, b, s, id)

	case VerbSandbox:
		spec, err := sandboxSpec(s, b)
		if err != nil {
			return nil, err
		}
		// k.Sandbox is Step-checkpointed internally under id, so it is called
		// directly rather than wrapped in another k.Step here.
		res, err := k.Sandbox(ctx, id, spec)
		if err != nil {
			return nil, err
		}
		return sandboxBinding(res), nil
	}
	return nil, fmt.Errorf("unknown verb %q", s.Verb)
}

// runForeach runs steps once per item in in:, each iteration under its own
// checkpoint namespace.
//
// Without the iteration index in the id, every pass through the loop would
// reuse item zero's checkpoint names — a retry after item three failed would
// find item one's steps already "done" and skip straight past them, silently
// dropping the rest of the list. The index makes each iteration's steps as
// distinct as if they had been written out by hand N times.
func runForeach(ctx context.Context, r *Recipe, k loopkit.Kit, b Bindings, s Step, id string) error {
	items, err := foreachItems(s, b)
	if err != nil {
		return err
	}
	name := s.Args["as"]
	for i, item := range items {
		iter := make(Bindings, len(b)+1)
		for key, v := range b {
			iter[key] = v
		}
		iter[name] = item
		if err := runSteps(ctx, r, k, iter, s.Steps, fmt.Sprintf("%s-%d-", id, i)); err != nil {
			return err
		}
	}
	return nil
}

// foreachItems resolves in: to a list of strings: a literal YAML list, or a
// template that renders a JSON array (typically a prior step's HTTP/tool
// output).
func foreachItems(s Step, b Bindings) ([]string, error) {
	if len(s.In) > 0 {
		out := make([]string, len(s.In))
		for i, v := range s.In {
			rendered, err := render(v, b)
			if err != nil {
				return nil, err
			}
			out[i] = rendered
		}
		return out, nil
	}
	raw, err := render(s.Args["in"], b)
	if err != nil {
		return nil, err
	}
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "<no value>" {
		return nil, nil
	}
	var items []string
	if err := json.Unmarshal([]byte(raw), &items); err == nil {
		return items, nil
	}
	// Not a JSON string array — try a general array (numbers, say) and
	// stringify each element, so a list of ticket ids still works.
	var anyItems []any
	if err := json.Unmarshal([]byte(raw), &anyItems); err == nil {
		out := make([]string, len(anyItems))
		for i, v := range anyItems {
			out[i] = fmt.Sprint(v)
		}
		return out, nil
	}
	return nil, fmt.Errorf("%q is not a JSON list — foreach's 'in' needs a literal list or a template that renders one", raw)
}

// awaitSpec builds an AwaitSpec from a step's fields, rendering each one.
func awaitSpec(s Step, b Bindings) (loopkit.AwaitSpec, error) {
	event, err := render(s.Args["event"], b)
	if err != nil {
		return loopkit.AwaitSpec{}, err
	}
	caseID, err := render(s.Args["case"], b)
	if err != nil {
		return loopkit.AwaitSpec{}, err
	}
	match := make(map[string]string, len(s.Match))
	for k, v := range s.Match {
		rendered, err := render(v, b)
		if err != nil {
			return loopkit.AwaitSpec{}, err
		}
		match[k] = rendered
	}
	timeout, err := renderDuration("timeout", s.Args["timeout"], b)
	if err != nil {
		return loopkit.AwaitSpec{}, err
	}
	return loopkit.AwaitSpec{Event: event, Match: match, CaseID: caseID, Timeout: timeout}, nil
}

// sandboxSpec builds a SandboxSpec from a step's fields. env.KEY fields are
// the container's environment, the same flattening http uses for header.NAME.
func sandboxSpec(s Step, b Bindings) (loopkit.SandboxSpec, error) {
	field := func(key string) (string, error) { return render(s.Args[key], b) }
	repo, err := field("repo")
	if err != nil {
		return loopkit.SandboxSpec{}, err
	}
	branch, err := field("branch")
	if err != nil {
		return loopkit.SandboxSpec{}, err
	}
	task, err := field("task")
	if err != nil {
		return loopkit.SandboxSpec{}, err
	}
	image, err := field("image")
	if err != nil {
		return loopkit.SandboxSpec{}, err
	}
	caseID, err := field("case")
	if err != nil {
		return loopkit.SandboxSpec{}, err
	}
	env := map[string]string{}
	for key, v := range s.Args {
		name, ok := strings.CutPrefix(key, "env.")
		if !ok {
			continue
		}
		rendered, err := render(v, b)
		if err != nil {
			return loopkit.SandboxSpec{}, err
		}
		env[name] = rendered
	}
	timeout, err := renderDuration("timeout", s.Args["timeout"], b)
	if err != nil {
		return loopkit.SandboxSpec{}, err
	}
	return loopkit.SandboxSpec{CaseID: caseID, Repo: repo, Branch: branch, Task: task,
		Image: image, Env: env, Timeout: timeout}, nil
}

// renderDuration parses an optional duration field; empty renders as zero,
// which both AwaitSpec and SandboxSpec document as "no timeout" — a real
// choice, not a missing one.
func renderDuration(field, raw string, b Bindings) (time.Duration, error) {
	rendered, err := render(raw, b)
	if err != nil {
		return 0, err
	}
	rendered = strings.TrimSpace(rendered)
	if rendered == "" {
		return 0, nil
	}
	d, err := time.ParseDuration(rendered)
	if err != nil {
		return 0, fmt.Errorf("%q is not a duration for %q (try 30m, 6h, 168h)", rendered, field)
	}
	return d, nil
}

// caseBinding is what {{ .c.id }}, {{ .c.state }} etc. resolve against.
// text/template matches map keys exactly, so this is lower_snake_case rather
// than the Go field names on loopkit.Case.
func caseBinding(c loopkit.Case) map[string]any {
	return map[string]any{
		"id": c.ID, "key": c.Key, "agent": c.Agent, "title": c.Title,
		"state": c.State, "namespace": c.Namespace,
		"thread_channel": c.ThreadChannel, "thread_ts": c.ThreadTS,
	}
}

func sandboxBinding(res loopkit.SandboxResult) map[string]any {
	return map[string]any{
		"run_id": res.RunID, "status": res.Status,
		"exit_code": res.ExitCode, "log_tail": res.LogTail,
	}
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
