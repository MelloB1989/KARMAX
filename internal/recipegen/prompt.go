package recipegen

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/MelloB1989/karmax/internal/recipes"
)

// recipeVerbs asks package recipes what verbs currently exist instead of
// this file freezing a copy of them. recipe.go's verb table is unexported, so
// the only place it is visible from outside the package is the "unknown
// step" error's Fix — this deliberately triggers that error and reads the
// list back out of it. A verb added to recipe.go's constants shows up here,
// and in the prompt, without anyone having to remember this package exists.
func recipeVerbs() []string {
	const probeKey = "__recipegen_unknown_verb_probe__"
	_, err := recipes.Parse("probe.yaml", []byte("steps:\n  - "+probeKey+": x\n"))
	var rerr *recipes.Error
	if !errors.As(err, &rerr) {
		return nil
	}
	const prefix = "the verbs are "
	i := strings.Index(rerr.Fix, prefix)
	if i < 0 {
		return nil
	}
	parts := strings.Split(rerr.Fix[i+len(prefix):], ", ")
	for i, p := range parts {
		parts[i] = strings.TrimSpace(p)
	}
	return parts
}

// verbDocs gives each verb's argument shape, for the system prompt. A verb
// missing here still appears in the prompt (see systemPrompt) — degraded to
// "no documented shape" rather than silently dropped — which is what keeps a
// forgotten entry a documentation gap instead of a generation bug.
var verbDocs = map[string]string{
	recipes.VerbAsk:      `- ask: "<prompt>" — the operator's agent, full tools and judgement. Binds with 'as'.`,
	recipes.VerbObserve:  `- observe: "<prompt>" — like ask, but every way to send is withheld. Binds with 'as'.`,
	recipes.VerbHarness:  `- harness: "<prompt>" — a coding harness: shell, files, web research. Binds with 'as'.`,
	recipes.VerbGateway:  `- gateway: "<prompt>" — the main model directly, no agent loop. Cheapest; try this first. Binds with 'as'.`,
	recipes.VerbHTTP:     `- http: { url: ..., method: GET, body: ..., header.X: ... } — headers are flat fields prefixed "header.". Binds with 'as'.`,
	recipes.VerbTool:     `- tool: { name: <tool-name>, ...other fields become its input } — binds with 'as'.`,
	recipes.VerbRecall:   `- recall: "<query>" — reads long-term memory. Binds with 'as'.`,
	recipes.VerbRemember: `- remember: { fact: ... } — WRITES to long-term memory.`,
	recipes.VerbNotify:   `- notify: { title: ..., body: ... } — an informational notification to the operator.`,
	recipes.VerbPropose:  `- propose: { title: ..., action: ..., summary: ... } — asks the operator to approve 'action' before it runs (title, action required; summary optional). Add to_role to ask a ROLE instead of the operator.`,
	recipes.VerbRemind:   `- remind: { title: ..., due: ..., notes: ... } — puts something on the operator's list; no approval needed.`,
	recipes.VerbSend:     `- send: { to: ..., text: ... } — SENDS a message. 'to' alone is a WhatsApp target. For a shared place instead, add channel: (and optionally thread:, e.g. a case's thread_ts) — "#eng" works as either 'to' or 'channel'.`,
	recipes.VerbSleep:    `- sleep: "<duration>" (e.g. "30m", "72h") — durable wait; the run resumes later, even across a restart.`,
	recipes.VerbLog:      `- log: "<message>" — writes a line to KARMAX's own log. Not seen by the operator.`,

	recipes.VerbCaseOpen:  `- case.open: { key: ..., title: ... } — opens or rejoins the case for key (idempotent; both fields required). Binds with 'as' (fields: id, key, agent, title, state, namespace, thread_channel, thread_ts).`,
	recipes.VerbCaseGet:   `- case.get: { key: ... } — looks a case up without creating it. Binds with 'as' (same fields as case.open; all empty if not found).`,
	recipes.VerbCaseState: `- case.state: { case: <case id>, state: ... } — moves the case's state along (both fields required).`,
	recipes.VerbCaseLog:   `- case.log: { case: <case id>, kind: ..., payload: ... } — appends to the case's history (all three required).`,
	recipes.VerbAwait:     `- await: { event: ..., case: <case id>, timeout: ..., match: { field: value, ... } } — parks the run, durably (survives a restart), until a matching event arrives (event required; match optional but near-always wanted). Binds with 'as' — the matched event's own payload, which is EXTERNAL data.`,
	recipes.VerbForeach:   `- foreach: { in: "{{ .list }}", as: <item binding>, steps: [ ...steps... ] } — runs its nested steps once per item (all three required). No top-level 'as' — foreach itself returns nothing.`,
	recipes.VerbSandbox:   `- sandbox: { repo: ..., branch: ..., task: ..., case: <case id>, timeout: ... } — hands a coding task to a container (repo, branch, task required; case optional but ties the run to one). Binds with 'as' (fields: run_id, status, exit_code, log_tail).`,
}

const schemaPreamble = `You write KARMAX recipes: one YAML file, nothing else. Reply with the YAML
only — no markdown code fence, no commentary before or after it.

Shape:

    name: some-descriptive-name        # required; no spaces or slashes
    on:
      schedule: "0 9 * * *"            # cron, 5 or 6 fields, or "@every 45m" / "@daily"
      # ...or event: "some.event.kind"
      # ...or webhook: "some-webhook-name"
      # ...or manual: true
      # exactly one of the above
    grants:
      - tool:whatsapp.read             # optional: what this recipe may hold
    steps:
      - <verb>: <text-or-fields>
        as: binding-name               # optional; only on a verb that returns something
        when: "{{ .binding }}"         # optional; guards the step
        else:
          - <verb>: ...                # optional; only meaningful together with 'when'

Rules:
- Exactly ONE verb per step. Never put two verbs in the same step.
- A verb's value is either a single string ("- log: hello") or a set of
  fields ("- notify:\n    title: ...\n    body: ..."). Use the shape given
  for that verb below; do not invent fields it does not document.
- Refer to an earlier step's result, or the trigger's own data, with
  {{ .binding }} — Go text/template syntax. Built-in bindings: {{ .trigger }}
  (what fired the run), {{ .payload }} (its data), {{ .now }}, {{ .today }}.
- 'as' is only legal on a verb that returns something — using it elsewhere is
  refused at parse time, not silently ignored.
- 'else' requires a 'when' on the same step.
- Use ONLY the verbs listed below. Do not invent one.`

const workedExamples = `Worked examples.

1) A scheduled digest:

    name: morning-digest
    on:
      schedule: "0 30 8 * * *"
    steps:
      - recall: "anything pending from yesterday"
        as: ctx
      - ask: "Write a short morning briefing using this context: {{ .ctx }}"
        as: briefing
      - notify:
          title: "Morning briefing"
          body: "{{ .briefing }}"

2) A webhook that asks before acting:

    name: refund-request
    on:
      webhook: refund-request
    steps:
      - gateway: "Summarise this refund request in one line: {{ .payload }}"
        as: summary
      - propose:
          title: "Refund request"
          summary: "{{ .summary }}"
          action: "Process the refund described above."

3) An org workflow that opens a case, waits on it being prioritised, then
   hands work to a sandbox — 'as' on case.open binds the case's own fields,
   so later steps address it by {{ .c.id }}, not by the ticket key again:

    name: ticket-to-build
    on:
      event: jira.issue.created
    steps:
      - case.open:
          key: "jira:{{ .ticket }}"
          title: "{{ .summary }}"
        as: c
      - await:
          event: jira.issue.updated
          match: { key: "{{ .ticket }}", status: Prioritized }
          case: "{{ .c.id }}"
          timeout: 168h
      - case.state:
          case: "{{ .c.id }}"
          state: building
      - sandbox:
          case: "{{ .c.id }}"
          repo: acme/api
          branch: main
          task: "Implement {{ .ticket }}: {{ .summary }}"
          timeout: 45m
        as: build
      - send:
          to: "#eng"
          thread: "{{ .c.thread_ts }}"
          text: "Build finished for {{ .ticket }}: {{ .build.status }}"

foreach is the one verb whose value nests OTHER steps, for repeating a body
once per item:

    - foreach:
        in: "{{ .tickets }}"
        as: t
        steps:
          - ask: "summarise {{ .t }}"
            as: summary
          - case.log: { case: "{{ .c.id }}", kind: note, payload: "{{ .summary }}" }
`

// systemPrompt is the full instruction set handed to the model: schema,
// every current verb with its shape, and worked examples. The verb section
// is built from recipeVerbs() precisely so this never has to be edited by
// hand when recipe.go grows one.
func systemPrompt() string {
	var b strings.Builder
	b.WriteString(schemaPreamble)
	b.WriteString("\n\nVerbs you may use — and nothing else:\n")
	for _, v := range recipeVerbs() {
		if doc, ok := verbDocs[v]; ok {
			b.WriteString(doc)
		} else {
			fmt.Fprintf(&b, "- %s: (see the recipe schema; no documented shape here yet)", v)
		}
		b.WriteString("\n")
	}
	b.WriteString("\n")
	b.WriteString(workedExamples)
	return b.String()
}

// draftPrompt turns a Request into the first thing the model sees.
func draftPrompt(req Request) string {
	var b strings.Builder
	b.WriteString("Write ONE recipe that does the following:\n\n")
	b.WriteString(strings.TrimSpace(req.Description))
	b.WriteString("\n")
	if req.Agent != "" {
		fmt.Fprintf(&b, "\nThis recipe runs on the agent %q.\n", req.Agent)
	}
	writeContext(&b, req.Context)
	return b.String()
}

// refinePrompt turns a RefineRequest into the first thing the model sees.
// The model is asked for the whole file, not a diff — a diff against
// something a model wrote itself is a second place for it to get the syntax
// wrong, for no benefit: Parse only ever sees the complete file anyway.
func refinePrompt(req RefineRequest) string {
	var b strings.Builder
	b.WriteString("Here is an existing recipe:\n\n")
	b.WriteString(strings.TrimSpace(req.YAML))
	b.WriteString("\n\nAmend it as follows. Reply with the COMPLETE recipe " +
		"— the whole file, not a diff:\n\n")
	b.WriteString(strings.TrimSpace(req.Instruction))
	b.WriteString("\n")
	if req.Agent != "" {
		fmt.Fprintf(&b, "\nThis recipe runs on the agent %q.\n", req.Agent)
	}
	writeContext(&b, req.Context)
	return b.String()
}

// retryPrompt feeds the located error straight back — path:line, message,
// and Fix — which is a shorter, more specific path to a correct next draft
// than resending the whole schema and hoping.
func retryPrompt(yaml string, err error) string {
	return fmt.Sprintf(
		"That did not parse. Fix ONLY the problem below and reply with the "+
			"complete corrected recipe — nothing else, no code fence, no explanation.\n\n"+
			"Your draft:\n%s\n\nThe error:\n%s\n", yaml, err.Error())
}

func writeContext(b *strings.Builder, ctx map[string]string) {
	if len(ctx) == 0 {
		return
	}
	b.WriteString("\nKnown details to use rather than invent:\n")
	for _, k := range sortedKeys(ctx) {
		fmt.Fprintf(b, "- %s: %s\n", k, ctx[k])
	}
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
