package recipes

import (
	"strings"
	"testing"
)

// The YAML surface, tested at the level an author actually hits it.
//
// A recipe is the tier for people who do not write Go, so the parser is the
// whole developer experience: what it accepts is the language, and what it says
// when it refuses is the documentation. Both are worth pinning.

// Five-field crontab is what the world writes, and it has to work.
//
// KARMAX's scheduler takes six fields. Two recipes in this package's own tests
// were written with five and could never have fired — which is how likely this
// mistake is, and why the parser normalises instead of refusing.
func TestSchedulesAcceptBothCronDialects(t *testing.T) {
	for _, tc := range []struct{ name, spec, want string }{
		{"five-field crontab", "0 9 * * *", "0 0 9 * * *"},
		{"six-field with seconds", "0 0 9 * * *", "0 0 9 * * *"},
		{"five-field with steps", "*/15 * * * *", "0 */15 * * * *"},
		{"six-field with steps", "0 */15 * * * *", "0 */15 * * * *"},
		{"an @every shorthand", "@every 45m", "@every 45m"},
		{"an @daily shorthand", "@daily", "@daily"},
		{"day-of-week by name", "0 9 * * MON", "0 0 9 * * MON"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, err := parse(t, "name: x\non:\n  schedule: \""+tc.spec+"\"\nsteps:\n  - log: hi\n")
			if err != nil {
				t.Fatalf("rejected a schedule that should run: %v", err)
			}
			if r.On.Schedule != tc.want {
				t.Errorf("schedule = %q, want %q — the scheduler is handed this verbatim",
					r.On.Schedule, tc.want)
			}
		})
	}
}

// A schedule the scheduler cannot run is caught HERE, not weeks later.
//
// Without this the recipe is accepted, `karmax recipe check` calls it valid,
// and the only symptom is that it never runs.
func TestAnImpossibleScheduleIsRefusedAtParseTime(t *testing.T) {
	for _, spec := range []string{
		"not a cron at all",
		"0 9 * *",           // four fields: neither dialect
		"99 * * * *",        // out of range
		"0 0 9 * * * *",     // seven fields
		"@every",            // shorthand with no interval
		"@every not-a-time", // shorthand with nonsense
	} {
		t.Run(spec, func(t *testing.T) {
			_, err := parse(t, "name: x\non:\n  schedule: \""+spec+"\"\nsteps:\n  - log: hi\n")
			if err == nil {
				t.Fatal("accepted a schedule that can never fire")
			}
			re, ok := err.(*Error)
			if !ok {
				t.Fatalf("error is not located: %v", err)
			}
			// The fix has to show the shape, since "invalid cron" tells an
			// author nothing they did not already suspect.
			if !strings.Contains(re.Fix, "*") {
				t.Errorf("the fix does not show a working example: %q", re.Fix)
			}
		})
	}
}

// A nested block under a step field is refused rather than silently emptied.
//
// The YAML walker reads scalar values, so a block arrives as "" and the step
// runs as though the field was never written. `headers:` under `http:` is the
// natural thing to write and the wrong thing, so it gets a real answer.
func TestANestedBlockUnderAStepIsRefused(t *testing.T) {
	_, err := parse(t, `
name: x
on:
  manual: true
steps:
  - http:
      url: https://example.com
      headers:
        Authorization: Bearer xyz
`)
	if err == nil {
		t.Fatal("accepted a nested block that would have been dropped")
	}
	re, ok := err.(*Error)
	if !ok {
		t.Fatalf("error is not located: %v", err)
	}
	if !strings.Contains(re.Fix, "header.") {
		t.Errorf("the fix does not name the flat form: %q", re.Fix)
	}
	if re.Line == 0 {
		t.Error("the error does not point at a line")
	}
}

// The flat header form is what the fix suggests, so it had better work.
func TestFlatHeadersParse(t *testing.T) {
	r := mustParse(t, `
name: x
on:
  manual: true
steps:
  - http:
      url: https://example.com
      header.Authorization: Bearer xyz
      header.Accept: application/json
`)
	args := r.Steps[0].Args
	if args["header.Authorization"] != "Bearer xyz" {
		t.Errorf("header not parsed: %#v", args)
	}
	if args["header.Accept"] != "application/json" {
		t.Errorf("second header not parsed: %#v", args)
	}
}

// Structural mistakes get a located error, not a Go panic or a stack trace.
func TestStructuralMistakesAreReportedNotCrashed(t *testing.T) {
	for _, tc := range []struct{ name, yaml, wants string }{
		{
			name:  "a tab instead of spaces",
			yaml:  "name: x\non:\n\tmanual: true\nsteps:\n  - log: hi\n",
			wants: "",
		},
		{
			name:  "steps is not a list",
			yaml:  "name: x\non:\n  manual: true\nsteps:\n  log: hi\n",
			wants: "steps must be a list",
		},
		{
			name:  "a step is a bare string",
			yaml:  "name: x\non:\n  manual: true\nsteps:\n  - just a string\n",
			wants: "must be a mapping",
		},
		{
			name:  "a step is an empty mapping",
			yaml:  "name: x\non:\n  manual: true\nsteps:\n  - {}\n",
			wants: "no action",
		},
		{
			name:  "the file is entirely comments",
			yaml:  "# nothing here\n# really\n",
			wants: "empty",
		},
		{
			name:  "the file is empty",
			yaml:  "",
			wants: "empty",
		},
		{
			name:  "a verb given a list",
			yaml:  "name: x\non:\n  manual: true\nsteps:\n  - log:\n      - a\n      - b\n",
			wants: "needs text or a set of fields",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parse(t, tc.yaml)
			if err == nil {
				t.Fatal("accepted a recipe that is not one")
			}
			re, ok := err.(*Error)
			if !ok {
				t.Fatalf("error is not located: %v", err)
			}
			if tc.wants != "" && !strings.Contains(re.Message, tc.wants) && !strings.Contains(re.Fix, tc.wants) {
				t.Errorf("neither message %q nor fix %q mentions %q", re.Message, re.Fix, tc.wants)
			}
			if re.Line <= 0 {
				t.Errorf("error has no line number: %+v", re)
			}
		})
	}
}

// Content survives parsing intact: block scalars, unicode, quoting and the
// template syntax a recipe is mostly made of.
func TestContentSurvivesParsing(t *testing.T) {
	r := mustParse(t, `
name: content
on:
  manual: true
steps:
  - harness: >
      A folded block that runs over
      several lines.
    as: out
  - notify:
      title: "☕ Digest — ünïcode"
      body: |
        Line one
        Line two
  - log: "braces {{ .out }} and a colon: here"
`)
	if got := r.Steps[0].Text; !strings.Contains(got, "several lines.") || strings.Contains(got, "\n  ") {
		t.Errorf("folded block not folded: %q", got)
	}
	if got := r.Steps[1].Args["title"]; got != "☕ Digest — ünïcode" {
		t.Errorf("unicode mangled: %q", got)
	}
	// A literal block keeps its newlines; that is the difference from folded.
	if got := r.Steps[1].Args["body"]; !strings.Contains(got, "Line one\nLine two") {
		t.Errorf("literal block lost its newlines: %q", got)
	}
	if got := r.Steps[2].Text; !strings.Contains(got, "{{ .out }}") {
		t.Errorf("template syntax mangled: %q", got)
	}
}

// Windows line endings are not a syntax error.
func TestCRLFIsAccepted(t *testing.T) {
	r, err := parse(t, "name: x\r\non:\r\n  manual: true\r\nsteps:\r\n  - log: hi\r\n")
	if err != nil {
		t.Fatalf("a file saved on Windows was rejected: %v", err)
	}
	if got := r.Steps[0].Text; got != "hi" {
		t.Errorf("text = %q, want %q — a stray carriage return is in there", got, "hi")
	}
}

// Several triggers on one recipe is allowed: a digest that runs on a schedule
// and can also be triggered by hand is an obvious thing to want.
func TestARecipeMayHaveSeveralTriggers(t *testing.T) {
	r := mustParse(t, `
name: x
on:
  schedule: "@daily"
  manual: true
  event: comms.message
steps:
  - log: hi
`)
	if r.On.Schedule == "" || !r.On.Manual || r.On.Event == "" {
		t.Errorf("a trigger was dropped: %+v", r.On)
	}
}

// enabled is a tri-state: absent means yes, which is not the same as false.
func TestEnabledDefaultsToOnAndCanBeTurnedOff(t *testing.T) {
	base := "name: x\non:\n  manual: true\nsteps:\n  - log: hi\n"
	if r := mustParse(t, base); !r.IsEnabled() {
		t.Error("a recipe with no 'enabled' should run")
	}
	if r := mustParse(t, "enabled: false\n"+base); r.IsEnabled() {
		t.Error("enabled: false did not disable it")
	}
	if r := mustParse(t, "enabled: true\n"+base); !r.IsEnabled() {
		t.Error("enabled: true disabled it")
	}
}

// else nests, and a mistake inside a nested branch is still reported.
func TestNestedBranchesAreValidatedToo(t *testing.T) {
	_, err := parse(t, `
name: x
on:
  manual: true
steps:
  - when: "{{ .x }}"
    log: outer
    else:
      - when: "{{ .y }}"
        log: inner
        else:
          - notify:
              body: "no title here"
`)
	if err == nil {
		t.Fatal("a mistake two branches deep was not reported")
	}
	if re, ok := err.(*Error); !ok || !strings.Contains(re.Message, "title") {
		t.Errorf("wrong error for a nested mistake: %v", err)
	}
}

// The same verb twice in one step is still two actions, and the order between
// them would be whatever the YAML walker happened to do.
func TestTheSameVerbTwiceIsRefused(t *testing.T) {
	_, err := parse(t, "name: x\non:\n  manual: true\nsteps:\n  - log: one\n    log: two\n")
	if err == nil {
		t.Fatal("accepted a step with the same verb twice")
	}
}

// grants are read, since they are what an operator approves.
func TestGrantsParse(t *testing.T) {
	r := mustParse(t, `
name: x
on:
  manual: true
grants:
  - tool:whatsapp.read
  - memory:nexus:write
steps:
  - log: hi
`)
	if len(r.Grants) != 2 || r.Grants[0] != "tool:whatsapp.read" {
		t.Errorf("grants = %#v", r.Grants)
	}
}

// Every verb parses in both the forms the language offers, so no verb is
// accidentally map-only or string-only.
func TestEveryVerbParsesInItsDocumentedForm(t *testing.T) {
	// The map form for each verb that requires fields, with those fields.
	mapForms := map[string]string{
		VerbHTTP:      "url: https://example.com",
		VerbTool:      "name: whatsapp.read",
		VerbNotify:    "title: t",
		VerbPropose:   "title: t\n      action: a",
		VerbRemind:    "title: t",
		VerbSend:      "to: x\n      text: y",
		VerbRemember:  "fact: something",
		VerbCaseOpen:  "key: k\n      title: t",
		VerbCaseGet:   "key: k",
		VerbCaseState: "case: c\n      state: s",
		VerbCaseLog:   "case: c\n      kind: note\n      payload: p",
		VerbAwait:     "event: e",
		VerbSandbox:   "repo: o/r\n      branch: main\n      task: t",
	}
	for verb, fields := range mapForms {
		t.Run(verb+" as fields", func(t *testing.T) {
			r := mustParse(t, "name: x\non:\n  manual: true\nsteps:\n  - "+verb+":\n      "+fields+"\n")
			if r.Steps[0].Verb != verb {
				t.Errorf("verb = %q, want %q", r.Steps[0].Verb, verb)
			}
		})
	}
	// The string form for the verbs that take one value.
	for _, verb := range []string{VerbAsk, VerbHarness, VerbGateway, VerbRecall, VerbLog, VerbSleep} {
		t.Run(verb+" as text", func(t *testing.T) {
			r := mustParse(t, "name: x\non:\n  manual: true\nsteps:\n  - "+verb+": something\n")
			if r.Steps[0].Verb != verb || r.Steps[0].Text != "something" {
				t.Errorf("parsed as %+v", r.Steps[0])
			}
		})
	}
}

// Line numbers are what make an error useful, so they survive nesting.
func TestStepsKeepTheirLineNumbers(t *testing.T) {
	r := mustParse(t, `
name: x
on:
  manual: true
steps:
  - log: first
  - when: "{{ .a }}"
    log: second
    else:
      - log: third
`)
	if r.Steps[0].Line != 6 {
		t.Errorf("first step at line %d, want 6", r.Steps[0].Line)
	}
	if r.Steps[1].Line != 7 {
		t.Errorf("second step at line %d, want 7", r.Steps[1].Line)
	}
	if len(r.Steps[1].Else) == 0 || r.Steps[1].Else[0].Line != 10 {
		t.Errorf("nested step line = %+v, want 10", r.Steps[1].Else)
	}
}

// case.open binds a Case, and {{ .c.id }} etc. only work if the fields it
// exposes to the template renderer are literally lower_snake_case — a Go
// struct's exported fields would not match at all.
func TestCaseOpenParsesAndBindsLowerCaseFields(t *testing.T) {
	r := mustParse(t, `
name: x
on:
  manual: true
steps:
  - case.open:
      key: "jira:{{ .ticket }}"
      title: "{{ .summary }}"
    as: c
`)
	s := r.Steps[0]
	if s.Verb != VerbCaseOpen || s.As != "c" {
		t.Fatalf("parsed as %+v", s)
	}
	if s.Args["key"] != "jira:{{ .ticket }}" {
		t.Errorf("key = %q", s.Args["key"])
	}
}

// await's match: is the one field in the language that is a nested block on
// purpose — several conditions, not one value — so it needs its own parsing
// path rather than the generic scalar-fields walker.
func TestAwaitParsesMatchAsAMap(t *testing.T) {
	r := mustParse(t, `
name: x
on:
  manual: true
steps:
  - await:
      event: jira.issue.updated
      match: { key: "{{ .ticket }}", status: Prioritized }
      timeout: 168h
    as: moved
`)
	s := r.Steps[0]
	if s.Verb != VerbAwait || s.As != "moved" {
		t.Fatalf("parsed as %+v", s)
	}
	if s.Args["event"] != "jira.issue.updated" || s.Args["timeout"] != "168h" {
		t.Errorf("scalar fields lost: %+v", s.Args)
	}
	if s.Match["key"] != "{{ .ticket }}" || s.Match["status"] != "Prioritized" {
		t.Errorf("match = %+v", s.Match)
	}
}

// A match that is not a set of field: value pairs gets a located error
// naming what "match" needs, not a generic "block" complaint.
func TestAwaitMatchNotAMapIsRefused(t *testing.T) {
	_, err := parse(t, `
name: x
on:
  manual: true
steps:
  - await:
      event: e
      match:
        - a
        - b
`)
	if err == nil {
		t.Fatal("accepted a match: that is a list, not field: value pairs")
	}
	re, ok := err.(*Error)
	if !ok {
		t.Fatalf("error is not located: %v", err)
	}
	if !strings.Contains(re.Message, "match") || re.Fix == "" {
		t.Errorf("message %q / fix %q does not point at match", re.Message, re.Fix)
	}
}

// foreach's steps: is a recipe inside a recipe — the other block-shaped field
// — and its own steps keep their line numbers just like the top-level list.
func TestForeachParsesANestedStepsList(t *testing.T) {
	r := mustParse(t, `
name: x
on:
  manual: true
steps:
  - foreach:
      in: "{{ .tickets }}"
      as: t
      steps:
        - ask: "summarise {{ .t }}"
          as: out
`)
	s := r.Steps[0]
	if s.Verb != VerbForeach {
		t.Fatalf("parsed as %+v", s)
	}
	if s.Args["as"] != "t" || s.Args["in"] != "{{ .tickets }}" {
		t.Errorf("foreach fields = %+v", s.Args)
	}
	if len(s.Steps) != 1 || s.Steps[0].Verb != VerbAsk || s.Steps[0].As != "out" {
		t.Fatalf("nested steps = %+v", s.Steps)
	}
}

// in: also accepts a literal list, for a recipe that already knows its items
// rather than computing them from an earlier step.
func TestForeachAcceptsALiteralInList(t *testing.T) {
	r := mustParse(t, `
name: x
on:
  manual: true
steps:
  - foreach:
      in: ["a", "b", "c"]
      as: x
      steps:
        - log: "{{ .x }}"
`)
	s := r.Steps[0]
	if len(s.In) != 3 || s.In[1] != "b" {
		t.Errorf("in = %+v", s.In)
	}
}

// A foreach missing any of its three required fields is refused by name, not
// lumped into one generic "foreach is broken" message.
func TestForeachMissingFieldsAreRefused(t *testing.T) {
	cases := []struct {
		name, yaml, wants string
	}{
		{
			name:  "no in",
			yaml:  "- foreach:\n      as: t\n      steps:\n        - log: hi\n",
			wants: `"in"`,
		},
		{
			name:  "no as",
			yaml:  "- foreach:\n      in: \"{{ .xs }}\"\n      steps:\n        - log: hi\n",
			wants: `"as"`,
		},
		{
			name:  "no steps",
			yaml:  "- foreach:\n      in: \"{{ .xs }}\"\n      as: t\n",
			wants: `"steps"`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parse(t, "name: x\non:\n  manual: true\nsteps:\n  "+tc.yaml)
			if err == nil {
				t.Fatal("accepted an incomplete foreach")
			}
			re, ok := err.(*Error)
			if !ok {
				t.Fatalf("error is not located: %v", err)
			}
			if !strings.Contains(re.Message, tc.wants) {
				t.Errorf("message %q does not mention %s", re.Message, tc.wants)
			}
		})
	}
}

// A steps: under foreach that is not a list (someone writing a single step
// without the leading '- ') is refused rather than silently parsed as nothing.
func TestForeachStepsNotAListIsRefused(t *testing.T) {
	_, err := parse(t, `
name: x
on:
  manual: true
steps:
  - foreach:
      in: "{{ .xs }}"
      as: t
      steps:
        log: hi
`)
	if err == nil {
		t.Fatal("accepted a steps: that is not a list")
	}
	re, ok := err.(*Error)
	if !ok {
		t.Fatalf("error is not located: %v", err)
	}
	if !strings.Contains(re.Message, "steps") || !strings.Contains(re.Message, "list") {
		t.Errorf("message does not say steps must be a list: %q", re.Message)
	}
}

// send's old to:-only form is untouched: no channel, no thread, same meaning
// it always had.
func TestSendBackwardCompatibility(t *testing.T) {
	r := mustParse(t, `
name: x
on:
  manual: true
steps:
  - send:
      to: "+911234567890"
      text: "hi"
`)
	s := r.Steps[0]
	if s.Args["to"] != "+911234567890" || s.Args["channel"] != "" || s.Args["thread"] != "" {
		t.Errorf("send args = %+v", s.Args)
	}
}

// send needs 'to' or 'channel' — omitting both is refused, but a bare 'to'
// that names a channel (the contract's own example) is accepted.
func TestSendNeedsToOrChannel(t *testing.T) {
	_, err := parse(t, "name: x\non:\n  manual: true\nsteps:\n  - send:\n      text: hi\n")
	if err == nil {
		t.Fatal("accepted a send with neither to nor channel")
	}
	re, ok := err.(*Error)
	if !ok || !strings.Contains(re.Message, "to") || !strings.Contains(re.Message, "channel") {
		t.Fatalf("wrong error: %v", err)
	}

	r := mustParse(t, `
name: x
on:
  manual: true
steps:
  - send: { to: "#eng", thread: "{{ .c.thread_ts }}", text: "PR is up" }
`)
	if r.Steps[0].Args["channel"] != "" || r.Steps[0].Args["thread"] == "" {
		t.Errorf("send args = %+v", r.Steps[0].Args)
	}
}

// case.open's agent: is optional — the contract's own examples never set it,
// and CaseOpen falls back to the loop when it is empty.
func TestCaseOpenAgentIsOptional(t *testing.T) {
	r := mustParse(t, `
name: x
on:
  manual: true
steps:
  - case.open: { key: k, title: t }
    as: c
`)
	if r.Steps[0].Args["agent"] != "" {
		t.Errorf("agent = %q, want empty", r.Steps[0].Args["agent"])
	}
	r2 := mustParse(t, `
name: x
on:
  manual: true
steps:
  - case.open: { agent: ops-pack, key: k, title: t }
    as: c
`)
	if r2.Steps[0].Args["agent"] != "ops-pack" {
		t.Errorf("agent = %q, want ops-pack", r2.Steps[0].Args["agent"])
	}
}

// case.say and case.history round out the case verbs: speaking into the
// case's own thread, and reading what already happened on it.
func TestCaseSayAndCaseHistoryParse(t *testing.T) {
	r := mustParse(t, `
name: x
on:
  manual: true
steps:
  - case.say: { case: "{{ .c.id }}", channel: "#eng", text: "PR is up" }
  - case.history: { case: "{{ .c.id }}", limit: 10 }
    as: hist
`)
	if r.Steps[0].Verb != VerbCaseSay || r.Steps[0].Args["text"] != "PR is up" {
		t.Fatalf("case.say parsed as %+v", r.Steps[0])
	}
	if r.Steps[1].Verb != VerbCaseHistory || r.Steps[1].As != "hist" {
		t.Fatalf("case.history parsed as %+v", r.Steps[1])
	}
}

// case.say needs a case and something to say; case.history needs a case.
func TestCaseSayAndCaseHistoryRequiredFields(t *testing.T) {
	cases := []struct{ name, yaml, wants string }{
		{"say with no text", "- case.say:\n      case: c\n", `"case.say" needs "text"`},
		{"say with no case", "- case.say:\n      text: hi\n", `"case.say" needs "case"`},
		{"history with no case", "- case.history:\n      limit: 5\n", `"case.history" needs "case"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parse(t, "name: x\non:\n  manual: true\nsteps:\n  "+tc.yaml)
			if err == nil {
				t.Fatal("accepted an incomplete step")
			}
			re, ok := err.(*Error)
			if !ok || !strings.Contains(re.Message, tc.wants) {
				t.Fatalf("wrong error: %v (want %q)", err, tc.wants)
			}
		})
	}
}

// propose's old form (no to_role) still parses with nothing extra required.
func TestProposeBackwardCompatibility(t *testing.T) {
	r := mustParse(t, `
name: x
on:
  manual: true
steps:
  - propose:
      title: "Merge?"
      action: "merge it"
`)
	if r.Steps[0].Args["to_role"] != "" {
		t.Errorf("to_role = %q, want empty", r.Steps[0].Args["to_role"])
	}
}
