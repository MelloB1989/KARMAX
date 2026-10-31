package comms

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/MelloB1989/karmax/internal/bus"
	"github.com/MelloB1989/karmax/internal/store"
	"go.uber.org/zap"
)

// echoChannel is a channel that echoes the account's raw sends and can say
// what its own history holds.
type echoChannel struct {
	*fakeChannel
	sink func(Echo)
	sent map[string]bool
}

func (e *echoChannel) SetEchoSink(fn func(Echo)) { e.sink = fn }
func (e *echoChannel) SentSince(_ context.Context, target, content string, _ time.Time) (bool, error) {
	return e.sent[target+"|"+content], nil
}

func echoManager(t *testing.T) (*Manager, *echoChannel, *store.Store) {
	t.Helper()
	s, err := store.New(filepath.Join(t.TempDir(), "karmax.db"), zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	mgr := NewManager(bus.NewLog(s, store.DefaultWorkspace, zap.NewNop()), s, zap.NewNop())
	ch := &echoChannel{fakeChannel: newFakeChannel("whatsapp-main", "whatsapp"), sent: map[string]bool{}}
	if err := mgr.Register(ch, "agent-1"); err != nil {
		t.Fatal(err)
	}
	old := echoSettle
	echoSettle = 0
	t.Cleanup(func() { echoSettle = old })
	return mgr, ch, s
}

func outboundTo(t *testing.T, s *store.Store, target string) []store.StoredChannelMessage {
	t.Helper()
	rows, err := s.ListChannelMessages(target, 20)
	if err != nil {
		t.Fatal(err)
	}
	var out []store.StoredChannelMessage
	for _, r := range rows {
		if r.Direction == string(Outbound) {
			out = append(out, r)
		}
	}
	return out
}

// A raw send — seen only by its echo — is recorded, so the duplicate check,
// a turn's "did I already answer", and a cut-off delivery all see it. A send
// comms made itself is not recorded twice.
func TestARawSendIsRecordedFromItsEchoAndCommsOwnSendIsNotDoubled(t *testing.T) {
	mgr, ch, s := echoManager(t)
	at := time.Now()

	ch.sink(Echo{Target: "group-1@g.us", Content: "Here is the Northwind deck.", MessageID: "3EB0A1", At: at})
	waitRows(t, s, "group-1@g.us", 1)
	rows := outboundTo(t, s, "group-1@g.us")
	if rows[0].SenderID != ProducerWacli || rows[0].Content != "Here is the Northwind deck." {
		t.Errorf("recorded %+v, want the raw send under its own producer", rows[0])
	}
	if !IsAnswerProducer(ProducerWacli) {
		t.Error("a raw send by the agent's own shell is not counted as an answer")
	}

	// The same words again through comms are refused as a repeat: the raw
	// send already said them.
	if err := mgr.SendFrom("", "whatsapp-main", "group-1@g.us", "Here is the Northwind deck."); err == nil {
		t.Error("comms repeated what a raw send had just said")
	}

	// comms' own send, then its echo: one row.
	if err := mgr.SendFrom("", "whatsapp-main", "chat-7@s.whatsapp.net", "On it."); err != nil {
		t.Fatal(err)
	}
	ch.sink(Echo{Target: "chat-7@s.whatsapp.net", Content: "On it.", MessageID: "3EB0A2", At: time.Now()})
	time.Sleep(100 * time.Millisecond)
	if n := len(outboundTo(t, s, "chat-7@s.whatsapp.net")); n != 1 {
		t.Errorf("comms' own send was recorded %d times", n)
	}
}

// Whether something went out is asked of the channel's own history too: a
// send that reached WhatsApp and was cut off before comms recorded it.
func TestSentSinceAsksTheChannelToo(t *testing.T) {
	mgr, ch, _ := echoManager(t)
	since := time.Now().Add(-time.Minute)
	if mgr.SentSince("whatsapp-main", "chat-7", "Here is the deck.", since) {
		t.Fatal("found a message nothing sent")
	}
	ch.sent["chat-7|Here is the deck."] = true
	if !mgr.SentSince("whatsapp-main", "chat-7", "Here is the deck.", since) {
		t.Error("the channel's own history was not asked")
	}
	if mgr.SentSince("no-such-channel", "chat-7", "Here is the deck.", since) {
		t.Error("an unknown channel answered")
	}
}

func waitRows(t *testing.T, s *store.Store, target string, n int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for len(outboundTo(t, s, target)) < n {
		if time.Now().After(deadline) {
			t.Fatalf("%s has %d outbound rows, want %d", target, len(outboundTo(t, s, target)), n)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
