package runtime

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/MelloB1989/karmax/internal/voice"
	"github.com/MelloB1989/karmax/pkg/karmahelper"
	"go.uber.org/zap"
)

func endBrain(t *testing.T, verdict func(context.Context, any) (float64, error)) *voiceBrain {
	b := newTestBrain(&fakeSession{reply: "Alright, bye."}, newVoiceLedger(testStore(t), "k", 1000, []string{"m"}, nil, zap.NewNop()))
	b.endVerdict, b.endAbove = verdict, 0.7
	return b
}

func answerEnd(t *testing.T, b *voiceBrain) voice.Reply {
	t.Helper()
	r, err := b.Answer(context.Background(), voice.Utterance{Text: "ok bye"})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestJevYesHangsUpAfterReply(t *testing.T) {
	r := answerEnd(t, endBrain(t, func(context.Context, any) (float64, error) { return 0.9, nil }))
	if !r.Hangup || r.Text == "" {
		t.Fatalf("reply = %+v, want goodbye then hangup", r)
	}
}

func TestJevNoDoesNotHangUp(t *testing.T) {
	if answerEnd(t, endBrain(t, func(context.Context, any) (float64, error) { return 0.2, nil })).Hangup {
		t.Fatal("hung up on a no")
	}
}

func TestJevErrorLeavesItToTheModelTool(t *testing.T) {
	b := endBrain(t, func(context.Context, any) (float64, error) { return 0, errors.New("down") })
	if answerEnd(t, b).Hangup {
		t.Fatal("hung up on a guess")
	}
	b.hangup.Store(true)
	if !answerEnd(t, b).Hangup {
		t.Fatal("the model's hangup tool was ignored")
	}
}

func TestEndVerdictRunsConcurrentlyWithTheTurn(t *testing.T) {
	b := endBrain(t, func(context.Context, any) (float64, error) {
		time.Sleep(300 * time.Millisecond)
		return 0.9, nil
	})
	b.session = &slowSession{fakeSession: fakeSession{reply: "Bye."}, d: 300 * time.Millisecond}
	start := time.Now()
	r := answerEnd(t, b)
	if took := time.Since(start); took > 500*time.Millisecond || !r.Hangup {
		t.Fatalf("took %v hangup %v, want ~300ms and hangup", took, r.Hangup)
	}
}

type slowSession struct {
	fakeSession
	d time.Duration
}

func (s *slowSession) Chat(ctx context.Context, m string) (string, []karmahelper.ToolCallRecord, karmahelper.TokenInfo, error) {
	time.Sleep(s.d)
	return s.fakeSession.Chat(ctx, m)
}

func TestToolTextIsNeverSpoken(t *testing.T) {
	names := []string{"call.hangup", "task.create"}
	for _, s := range []string{"call hangup", "Call.Hangup.", "call_hangup()", `task.create {"goal":"x"}`, `{"goal": "x"}`} {
		if !isToolText(s, names) {
			t.Errorf("%q should be dropped", s)
		}
	}
	for _, s := range []string{"I'll call you back.", "Thanks for your time.", "Let me hang up on that thought."} {
		if isToolText(s, names) {
			t.Errorf("%q must be spoken", s)
		}
	}
	var said []string
	ts := newTurnSpeech(func(s string) bool { said = append(said, s); return true }, false)
	ts.names = names
	ts.Write("Alright, hanging up now. call hangup")
	ts.Flush()
	if len(said) != 1 || said[0] != "Alright, hanging up now." {
		t.Fatalf("streamed %q", said)
	}
	if got := dropToolText("Thanks for your time. call hangup", names); got != "Thanks for your time." {
		t.Fatalf("final = %q", got)
	}
}
