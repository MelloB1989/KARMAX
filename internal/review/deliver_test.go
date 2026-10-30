package review

import (
	"path/filepath"
	"sync"
	"testing"

	"github.com/MelloB1989/karmax/internal/store"
	"github.com/MelloB1989/karmax/internal/tools/builtin"
	"go.uber.org/zap"
)

// A staleness check-in is never urgent, and the operator's WhatsApp is for
// approvals and urgent items only. It went to WhatsApp directly on top of the
// app every 45 minutes. It is filed in the app, where the operator answers it
// (or answers it anywhere: the agent sees open reviews every turn).
func TestACheckInIsFiledInTheAppNotSentToWhatsApp(t *testing.T) {
	s, err := store.New(filepath.Join(t.TempDir(), "k.db"), zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })

	var mu sync.Mutex
	var whatsapp, mirrored []string
	builtin.SetMirrorHook(func(ev builtin.MirrorEvent) { mu.Lock(); mirrored = append(mirrored, ev.Title); mu.Unlock() })
	t.Cleanup(func() { builtin.SetMirrorHook(nil) })

	r := New(Config{AgentID: "nexus", Namespace: "nexus", WAChannelID: "wa", WATarget: "15555550100@s.whatsapp.net",
		SendFunc: func(_, _, content string) error {
			mu.Lock()
			whatsapp = append(whatsapp, content)
			mu.Unlock()
			return nil
		}},
		s, nil, zap.NewNop())
	r.deliver(store.StoredReview{ID: "r-1", Question: "Is the Contoso pilot still on for Friday?", Options: `["Yes","Cancelled"]`})

	mu.Lock()
	defer mu.Unlock()
	if len(whatsapp) != 0 {
		t.Errorf("a check-in was sent to WhatsApp: %v", whatsapp)
	}
	if len(mirrored) != 0 {
		t.Errorf("a check-in was offered to the WhatsApp mirror: %v", mirrored)
	}
	notes, _ := s.ListNotifications(10)
	if len(notes) != 1 {
		t.Fatalf("the check-in is not in the app: %+v", notes)
	}
}
