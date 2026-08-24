package recipes

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/MelloB1989/karmax/pkg/loopkit"
)

// Dry running.
//
// Non-programmers need to see what a recipe would do before they trust it with
// their WhatsApp account. Every side effect is recorded instead of performed,
// the clock is fake so a 72h sleep resolves instantly, and reads return
// obviously-fake values so nobody mistakes a rehearsal for a result.

// DryRun records what a recipe would have done.
type DryRun struct {
	Actions []string
	Now     time.Time
	trigger loopkit.Trigger
}

// NewDryRun builds a fake Kit fixed at a known time.
func NewDryRun(trigger loopkit.Trigger) *DryRun {
	return &DryRun{Now: time.Date(2026, 8, 10, 9, 0, 0, 0, time.UTC), trigger: trigger}
}

func (d *DryRun) record(format string, args ...any) {
	d.Actions = append(d.Actions, fmt.Sprintf(format, args...))
}

// Report renders the run for a terminal.
func (d *DryRun) Report() string {
	if len(d.Actions) == 0 {
		return "Nothing would happen — check the 'when:' conditions."
	}
	var b strings.Builder
	for i, a := range d.Actions {
		fmt.Fprintf(&b, "%2d. %s\n", i+1, a)
	}
	return b.String()
}

func (d *DryRun) Trigger() loopkit.Trigger { return d.trigger }

// Steps run for real in a dry run: they are the control flow, and skipping them
// would rehearse a different recipe.
func (d *DryRun) Step(name string, fn func() (string, error)) (string, error) { return fn() }
func (d *DryRun) Once(name string, fn func() error) error                     { return fn() }

func (d *DryRun) Ask(_ context.Context, prompt string) (string, error) {
	d.record("ask the agent: %s", oneLine(prompt))
	return "[the agent's answer would appear here]", nil
}

func (d *DryRun) Observe(_ context.Context, prompt string) (string, error) {
	d.record("ask the agent, with no way to send a message: %s", oneLine(prompt))
	return "[the agent's answer would appear here]", nil
}

func (d *DryRun) Harness(_ context.Context, prompt string) (string, error) {
	d.record("run the harness on: %s", oneLine(prompt))
	return "[the harness output would appear here]", nil
}

func (d *DryRun) Gateway(_ context.Context, prompt string, _ ...loopkit.Tool) (string, error) {
	d.record("ask the model: %s", oneLine(prompt))
	return "[the model's answer would appear here]", nil
}

func (d *DryRun) Summarize(_ context.Context, prompt string) (string, error) {
	d.record("summarise: %s", oneLine(prompt))
	return "[a summary would appear here]", nil
}

func (d *DryRun) Recall(query string, limit int) ([]string, error) {
	d.record("search memory for %q (up to %d)", oneLine(query), limit)
	return []string{"[a matching memory would appear here]"}, nil
}

func (d *DryRun) Remember(fact string) error {
	d.record("REMEMBER: %s", oneLine(fact))
	return nil
}

func (d *DryRun) Notify(title, body string) error {
	d.record("NOTIFY you — %q: %s", title, oneLine(body))
	return nil
}

func (d *DryRun) Propose(title, summary, action string) error {
	d.record("ASK YOUR APPROVAL — %q (%s); on approval it would: %s",
		title, oneLine(summary), oneLine(action))
	return nil
}

func (d *DryRun) Remind(title, due, notes string) error {
	d.record("REMIND you — %q due %s %s", title, orNone(due), oneLine(notes))
	return nil
}

func (d *DryRun) SendWhatsApp(_ context.Context, target, content string) error {
	d.record("SEND WhatsApp to %s: %s", target, oneLine(content))
	return nil
}

func (d *DryRun) ReadWhatsApp(_ context.Context, chat string, limit int) (string, error) {
	d.record("read %s WhatsApp messages from %s", plural(limit), orNone(chat))
	return "[recent messages would appear here]", nil
}

func (d *DryRun) HTTP(_ context.Context, method, url string, _ map[string]string, _ string) (string, int, error) {
	d.record("HTTP %s %s", method, url)
	return `{"note":"a real response would appear here"}`, 200, nil
}

func (d *DryRun) After(id string, delay time.Duration, _ map[string]any) error {
	d.record("WAIT %s, then continue from here (survives a restart) — resuming %s",
		delay, d.Now.Add(delay).Format("Mon 2 Jan 15:04"))
	return nil
}

func (d *DryRun) CancelAfter(id string) error {
	d.record("cancel the timer %q", id)
	return nil
}

// Sleep returns at once. A rehearsal that actually waits is a rehearsal nobody
// finishes watching.
func (d *DryRun) Sleep(_ context.Context, delay time.Duration) error {
	d.record("wait %s", delay)
	return nil
}

func (d *DryRun) Fence(source, content string) string {
	return "<untrusted-content source=\"" + source + "\">" + content + "</untrusted-content>"
}

func (d *DryRun) RunLoop(name string) error {
	d.record("trigger the loop %q", name)
	return nil
}

func (d *DryRun) Logf(format string, args ...any) {
	d.record("log: %s", fmt.Sprintf(format, args...))
}

func (d *DryRun) Config(key string) string    { return "" }
func (d *DryRun) HostTool(name string) string { return name }

func (d *DryRun) ChatSummary(jid string) (*loopkit.ChatSummaryRecord, error) { return nil, nil }
func (d *DryRun) SaveChatSummary(rec loopkit.ChatSummaryRecord) error        { return nil }

func (d *DryRun) ShortSet(group, key, value string, ttl time.Duration) error { return nil }
func (d *DryRun) ShortGet(group, key string) (string, bool, error)           { return "", false, nil }
func (d *DryRun) ShortAll(group string) ([]loopkit.ShortMemory, error)       { return nil, nil }
func (d *DryRun) ShortForget(group, key string) error                        { return nil }
func (d *DryRun) ShortClear(group string) error                              { return nil }

func (d *DryRun) CaseOpen(agent, key, title string) (loopkit.Case, error) {
	if agent == "" {
		agent = "this loop"
	}
	d.record("open the case %q for %s (%q) — or rejoin it, if it already exists", key, agent, oneLine(title))
	return loopkit.Case{
		ID: "[a case id would appear here]", Key: key, Title: title, State: "open",
		ThreadChannel: "[a channel would appear here]", ThreadTS: "[a thread id would appear here]",
	}, nil
}

func (d *DryRun) CaseSay(ctx context.Context, caseID, channel, text string) error {
	where := channel
	if where == "" {
		where = "the case's thread"
	}
	d.record("say in %s: %q", where, oneLine(text))
	return nil
}

func (d *DryRun) CaseGet(key string) (loopkit.Case, bool, error) {
	d.record("look up the case %q", key)
	return loopkit.Case{ID: "[a case id would appear here]", Key: key, State: "open"}, true, nil
}

func (d *DryRun) CaseSetState(caseID, state string) error {
	d.record("move case %s to state %q", caseID, state)
	return nil
}

func (d *DryRun) CaseLog(caseID, kind, payload string) error {
	d.record("log to case %s — %s: %s", caseID, kind, oneLine(payload))
	return nil
}

func (d *DryRun) CaseHistory(caseID string, limit int) ([]string, error) {
	d.record("read the last %s history line(s) for case %s", plural(limit), caseID)
	return []string{"[case history would appear here]"}, nil
}

// Await, in a real run, can park the process for days. A rehearsal that
// actually waited would never finish, so it reports what it would wait for
// and moves straight on rather than suspending.
func (d *DryRun) Await(_ context.Context, id string, spec loopkit.AwaitSpec) (map[string]any, error) {
	d.record("WAIT for %q matching %v%s", spec.Event, spec.Match, timeoutSuffix(spec.Timeout))
	return map[string]any{"note": "[the event payload would appear here]"}, nil
}

func (d *DryRun) SendTo(_ context.Context, channel, thread, content string) error {
	d.record("SEND to %s%s: %s", channel, threadSuffix(thread), oneLine(content))
	return nil
}

func (d *DryRun) ProposeTo(role, title, summary, action string) (string, error) {
	d.record("ASK APPROVAL from anyone holding %q — %q (%s); on approval it would: %s",
		role, title, oneLine(summary), oneLine(action))
	return "[a proposal id would appear here]", nil
}

func (d *DryRun) Sandbox(_ context.Context, id string, spec loopkit.SandboxSpec) (loopkit.SandboxResult, error) {
	d.record("RUN CODE in a container against %s@%s: %s", spec.Repo, spec.Branch, oneLine(spec.Task))
	return loopkit.SandboxResult{
		RunID: "[a run id would appear here]", Status: "exited", ExitCode: 0,
		LogTail: "[container output would appear here]",
	}, nil
}

func (d *DryRun) Audit(verb, target, decision, detail string) error { return nil }

func timeoutSuffix(d time.Duration) string {
	if d <= 0 {
		return " (no timeout)"
	}
	return fmt.Sprintf(" (times out after %s)", d)
}

func threadSuffix(thread string) string {
	if thread == "" {
		return ""
	}
	return " (thread " + thread + ")"
}

func oneLine(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 140 {
		return s[:140] + "…"
	}
	return s
}

func orNone(s string) string {
	if strings.TrimSpace(s) == "" {
		return "(unset)"
	}
	return s
}

func plural(n int) string {
	if n <= 0 {
		return "recent"
	}
	return fmt.Sprintf("%d", n)
}

// Compile-time proof that a rehearsal is the same shape as the real thing.
var _ loopkit.Kit = (*DryRun)(nil)
