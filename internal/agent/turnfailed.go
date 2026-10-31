package agent

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/MelloB1989/karmax/internal/bus"
	"github.com/MelloB1989/karmax/pkg/karmahelper"
	"github.com/MelloB1989/karmax/pkg/loopkit"
	"go.uber.org/zap"
)

// A failed turn, told truthfully.
//
// The operator used to get "something failed, please send it again" whatever
// the turn had done — including turns that had already started the work or
// written to someone as them, where sending it again does it twice. The
// notice now says what was already done, what failed, and whether to send it
// again.

// turnFailure is a turn whose brain failed, with the tool calls it had made
// before it did: they travel with the error so they can be told.
type turnFailure struct {
	calls []karmahelper.ToolCallRecord
	err   error
}

func (f *turnFailure) Error() string { return f.err.Error() }
func (f *turnFailure) Unwrap() error { return f.err }

// afterFailedTurn is what a failed turn still owes the operator: an answer in
// their own chat saying what happened, and, for any turn, the policy's report
// of what it sent as them before it failed.
func (a *Agent) afterFailedTurn(evt bus.Event, started time.Time, cause error) {
	a.tellOperatorTurnFailed(evt, started, cause)
	a.deliverTurnReport(evt, started.Truncate(time.Second), "")
}

// tellOperatorTurnFailed answers an operator message whose turn failed. A
// failed turn used to end in a log line and nothing else, so from the chat it
// looked exactly like being ignored. Monitored chats get nothing: a stranger
// should never see KARMAX's internals, and their loops decide replies anyway.
func (a *Agent) tellOperatorTurnFailed(evt bus.Event, started time.Time, cause error) {
	if evt.Kind != bus.EventCommsMessage || a.commsSend == nil {
		return
	}
	if a.ctx != nil && a.ctx.Err() != nil {
		return // shutting down; the journal replays the turn on restart
	}
	channelID, _ := evt.Payload["karmax_channel_id"].(string)
	target, _ := evt.Payload["channel_id"].(string)
	if channelID == "" || !a.isFromOperator(target) {
		return
	}
	if a.repliedDuringTurn(target, started) {
		return
	}
	done := a.doneDuring(started, cause)
	// What was done as the operator is told whatever the message was; only a
	// turn that did nothing asks whether the failure is worth a word.
	if len(done) == 0 && !a.shouldTellOperatorTurnFailed(evt, cause) {
		return
	}
	if err := a.sendStatus(channelID, target, turnFailedNotice(rootCause(cause), done)); err != nil {
		a.log.Warn("could not tell the operator a turn failed", zap.Error(err))
	}
}

// turnFailedNotice says what failed, what had already been done, and whether
// to send the message again.
func turnFailedNotice(why string, done []string) string {
	if why == "" {
		why = "something failed on my side"
	}
	if len(done) == 0 {
		return fmt.Sprintf("I couldn't handle that: %s. Nothing was done yet, so it's safe to send it again.", why)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "I couldn't finish that: %s.\nAlready done before it stopped:\n", why)
	for _, d := range done {
		b.WriteString("• " + d + "\n")
	}
	b.WriteString("So don't send it again as it was — that would do these twice. Tell me what's still missing, if anything.")
	return b.String()
}

// maxDoneLines bounds the actions a failure notice lists by name.
const maxDoneLines = 5

// doneDuring is what a failed turn had already done: what reached other
// people as the operator since it began (the outbound ledger, which every
// sending tool writes, whichever brain or tool sent it), then the actions it
// took — commands run by name, anything else counted. Calls that themselves
// failed did nothing and are left out.
func (a *Agent) doneDuring(started time.Time, cause error) []string {
	sent, _ := a.sentSince(started.Truncate(time.Second))
	done := append([]string(nil), sent...)

	var f *turnFailure
	if !errors.As(cause, &f) {
		return done
	}
	others := map[string]int{}
	seen := map[string]bool{}
	for _, tc := range f.calls {
		if tc.Error != nil || tc.Result.IsError {
			continue
		}
		line := describeAction(tc.Name, tc.Input)
		if line == "" {
			if cmd, _ := tc.Input["command"].(string); strings.TrimSpace(cmd) != "" {
				line = "ran `" + truncateStr(firstLineOf(cmd), 80) + "`"
			}
		}
		if line == "" || len(done) >= maxDoneLines {
			others[tc.Name]++
			continue
		}
		if !seen[line] {
			seen[line] = true
			done = append(done, line)
		}
	}
	if n := countOf(others); n > 0 {
		done = append(done, fmt.Sprintf("and %d other tool call(s) (%s)", n, namesOf(others)))
	}
	return done
}

// rootCause is a failure in its own words: the innermost error, without the
// layers that wrapped it on the way out.
func rootCause(err error) string {
	if err == nil {
		return ""
	}
	for {
		next := errors.Unwrap(err)
		if next == nil || strings.TrimSpace(next.Error()) == "" {
			break
		}
		err = next
	}
	return truncateStr(strings.TrimRight(strings.TrimSpace(err.Error()), "."), 200)
}

// shouldTellOperatorTurnFailed asks Jev; with no answer the operator is told.
func (a *Agent) shouldTellOperatorTurnFailed(evt bus.Event, cause error) bool {
	a.mu.RLock()
	d := a.decider
	a.mu.RUnlock()
	if d == nil {
		return true
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	msg, _ := evt.Payload["content"].(string)
	state := map[string]any{"operator_message": msg, "failure": rootCause(cause), "already_done": "nothing"}
	res, err := d(ctx, state, loopkit.Questions{
		"tell": loopkit.Noul("The operator sent this message and the turn handling it failed before replying, having done nothing. Should the operator be told it failed, so they can send it again?").
			When("the message expected a reply or an action", "the message needed no response, such as an acknowledgement or a reaction"),
	})
	if err != nil || res == nil {
		return true
	}
	return res.Yes("tell", 0.5)
}

func firstLineOf(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[:i]) + " …"
	}
	return s
}

func countOf(m map[string]int) int {
	n := 0
	for _, c := range m {
		n += c
	}
	return n
}

func namesOf(m map[string]int) string {
	names := make([]string, 0, len(m))
	for name, c := range m {
		if c > 1 {
			name = fmt.Sprintf("%s ×%d", name, c)
		}
		names = append(names, name)
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}
