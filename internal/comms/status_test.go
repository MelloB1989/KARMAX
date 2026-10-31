package comms

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/MelloB1989/karmax/internal/bus"
	"github.com/MelloB1989/karmax/internal/store"
	"go.uber.org/zap"
)

func statusTestManager(t *testing.T) (*Manager, *fakeChannel) {
	t.Helper()
	s, err := store.New(filepath.Join(t.TempDir(), "karmax.db"), zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	mgr := NewManager(bus.NewLog(s, store.DefaultWorkspace, zap.NewNop()), s, zap.NewNop())
	ch := newFakeChannel("wa", "whatsapp")
	if err := mgr.Register(ch, "nexus"); err != nil {
		t.Fatal(err)
	}
	return mgr, ch
}

// "On it" and "that failed, please send it again" are the same words every
// time by design, each about a different message. The duplicate guard, which
// refuses the same words to one person for six hours, took them for repeats:
// after one slow turn the operator's next slow turns got no ack, and a second
// failed turn within the window was silence — the very thing the failure
// notice exists to prevent.
func TestATurnStatusNoticeIsNotRefusedAsARepeat(t *testing.T) {
	mgr, ch := statusTestManager(t)
	const operator = "100000000000001@lid"

	for i := range 3 {
		if err := mgr.SendFrom(ProducerStatus, "wa", operator, "👀 on it…"); err != nil {
			t.Fatalf("status notice %d refused: %v", i+1, err)
		}
	}
	if n := len(ch.sentMessages()); n != 3 {
		t.Errorf("%d notices delivered, want 3", n)
	}
	// A status notice is never the turn's answer.
	if IsAnswerProducer(ProducerStatus) {
		t.Error("a status notice would count as the turn's reply")
	}
}

// Everything else is still said once.
func TestAnOrdinaryRepeatIsStillRefused(t *testing.T) {
	mgr, _ := statusTestManager(t)
	const operator = "100000000000001@lid"

	if err := mgr.Send("wa", operator, "The invoice went to the client."); err != nil {
		t.Fatal(err)
	}
	if err := mgr.Send("wa", operator, "The invoice went to the client."); !errors.Is(err, ErrDuplicateSend) {
		t.Fatalf("second identical send: err = %v, want ErrDuplicateSend", err)
	}
	if err := mgr.SendFrom(ProducerSystem, "wa", operator, "The invoice went to the client."); !errors.Is(err, ErrDuplicateSend) {
		t.Fatalf("a system alert is still deduplicated: err = %v, want ErrDuplicateSend", err)
	}
}
