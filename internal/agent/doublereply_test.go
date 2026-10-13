package agent

import (
	"errors"
	"github.com/MelloB1989/karmax/internal/tools"
	"path/filepath"
	"testing"
	"time"

	"github.com/MelloB1989/karmax/internal/comms"
	"github.com/MelloB1989/karmax/internal/store"
	"github.com/MelloB1989/karmax/pkg/karmahelper"
	"go.uber.org/zap"
)

func replyTestAgent(t *testing.T) *Agent {
	t.Helper()
	s, err := store.New(filepath.Join(t.TempDir(), "test.db"), zap.NewNop())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	return &Agent{store: s, log: zap.NewNop()}
}

func saveOutbound(t *testing.T, a *Agent, target, content string) {
	t.Helper()
	if err := a.store.SaveChannelMessage(store.StoredChannelMessage{
		ID:          target + content,
		ChannelID:   target,
		ChannelType: "whatsapp",
		Direction:   "outbound",
		Content:     content,
	}); err != nil {
		t.Fatalf("save outbound: %v", err)
	}
}

// The operator's DM, answered twice: the harness sent the real reply through
// its shell, and the fallback then delivered the harness's report of having
// sent it as a second message.
func TestAnAnswerAlreadySentIsNotDeliveredAgain(t *testing.T) {
	a := replyTestAgent(t)
	const target = "100000000000001@lid"

	turnStart := time.Now().Add(-time.Minute).Truncate(time.Second)
	saveOutbound(t, a, target, "Calculated it, excluding cab/Rapido: ₹4,310 total.")

	if !a.repliedDuringTurn(target, turnStart) {
		t.Fatal("a send made during the turn must count as the reply")
	}
}

// The ack is not an answer. Counting it as one would leave the operator with
// "on it" and nothing else.
func TestTheSlowTurnAckDoesNotCountAsTheReply(t *testing.T) {
	a := replyTestAgent(t)
	const target = "100000000000001@lid"

	turnStart := time.Now().Add(-time.Minute).Truncate(time.Second)
	saveOutbound(t, a, target, ackMessage)

	if a.repliedDuringTurn(target, turnStart) {
		t.Fatal("the ack must not suppress the real reply")
	}
}

// Yesterday's conversation says nothing about this turn.
func TestAnEarlierMessageIsNotThisTurnsReply(t *testing.T) {
	a := replyTestAgent(t)
	const target = "100000000000001@lid"

	saveOutbound(t, a, target, "Morning! LeetCode focus: Trees today.")
	turnStart := time.Now().Add(time.Second).Truncate(time.Second)

	if a.repliedDuringTurn(target, turnStart) {
		t.Fatal("a message sent before the turn began is not its reply")
	}
}

// A chat nobody has written to yet still gets its answer.
func TestAFreshChatIsNotTreatedAsAnswered(t *testing.T) {
	a := replyTestAgent(t)
	if a.repliedDuringTurn("15555550102@s.whatsapp.net", time.Now().Add(-time.Minute)) {
		t.Fatal("an empty history must not read as an answer")
	}
}

// The harness reaches comms.send through its shell, so the record is named
// "Bash" and only the command line identifies it.
func TestAShellCommsSendIsRecognised(t *testing.T) {
	send := karmahelper.ToolCallRecord{
		Name: "Bash",
		Input: map[string]any{
			"command": `karmax tool call comms.send target=100000000000001@lid content="done"`,
		},
	}
	if !commsSendShellCall(send) {
		t.Fatal("a shell comms.send must be recognised as a send")
	}

	lookup := karmahelper.ToolCallRecord{
		Name:  "Bash",
		Input: map[string]any{"command": `karmax memory search "phonepe"`},
	}
	if commsSendShellCall(lookup) {
		t.Fatal("a memory search is not a send")
	}

	if commsSendShellCall(karmahelper.ToolCallRecord{Name: "Read", Input: map[string]any{"file_path": "/comms.send"}}) {
		t.Fatal("only shell tools carry a command line")
	}
}

func saveOutboundFrom(t *testing.T, a *Agent, target, content, producer string) {
	t.Helper()
	if err := a.store.SaveChannelMessage(store.StoredChannelMessage{
		ID: target + content, ChannelID: target, ChannelType: "whatsapp",
		SenderID: producer, Direction: "outbound", Content: content,
	}); err != nil {
		t.Fatalf("save outbound: %v", err)
	}
}

// A loop's post to the same chat mid-turn (the org office's "Indexed 3
// repositories") used to be counted as the answer and swallow the real one.
func TestALoopOrSystemPostDoesNotCountAsTheReply(t *testing.T) {
	for _, producer := range []string{comms.ProducerLoop, comms.ProducerSystem} {
		a := replyTestAgent(t)
		const target = "100000000000001@lid"
		turnStart := time.Now().Add(-time.Minute).Truncate(time.Second)
		saveOutboundFrom(t, a, target, "📚 Indexed 3 repositories into the knowledge store.", producer)
		if a.repliedDuringTurn(target, turnStart) {
			t.Fatalf("a %s post must not suppress the agent's answer", producer)
		}
	}
}

// The agent's own send still counts even when a loop post sits beside it.
func TestAnAgentSendBesideALoopPostStillCounts(t *testing.T) {
	a := replyTestAgent(t)
	const target = "100000000000001@lid"
	turnStart := time.Now().Add(-time.Minute).Truncate(time.Second)
	saveOutboundFrom(t, a, target, "loop post", comms.ProducerLoop)
	saveOutboundFrom(t, a, target, "the answer", "")
	if !a.repliedDuringTurn(target, turnStart) {
		t.Fatal("an untagged (agent/shell) send must count as the reply")
	}
}

// "error: unknown tool: comms.send" delivered nothing; counting the attempt as
// a send skipped the fallback and the operator got no answer.
func TestAFailedShellCommsSendIsNotASend(t *testing.T) {
	failed := karmahelper.ToolCallRecord{
		Name:   "Bash",
		Input:  map[string]any{"command": `karmax tool call comms.send channel_id=chat message="hi"`},
		Result: tools.ToolResult{IsError: true},
		Error:  errors.New("tool call failed"),
	}
	if commsSendShellCall(failed) {
		t.Fatal("a failed comms.send must fall through to the fallback delivery")
	}
}
