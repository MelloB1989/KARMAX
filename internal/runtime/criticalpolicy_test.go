package runtime

import (
	"context"
	"path/filepath"
	"sync"
	"testing"

	"github.com/MelloB1989/karmax/internal/bus"
	"github.com/MelloB1989/karmax/internal/comms"
	"github.com/MelloB1989/karmax/internal/policy"
	"github.com/MelloB1989/karmax/internal/store"
	"go.uber.org/zap"
)

// fakeWhatsApp is a channel that records what it was asked to send.
type fakeWhatsApp struct {
	mu   sync.Mutex
	sent []string
}

func (f *fakeWhatsApp) ID() string                             { return "wa" }
func (f *fakeWhatsApp) Type() string                           { return "whatsapp" }
func (f *fakeWhatsApp) Start(context.Context) error            { return nil }
func (f *fakeWhatsApp) Stop() error                            { return nil }
func (f *fakeWhatsApp) IncomingMessages() <-chan comms.Message { return nil }
func (f *fakeWhatsApp) SendEmbed(context.Context, string, comms.Embed) error {
	return nil
}
func (f *fakeWhatsApp) SendFile(context.Context, string, string, []byte) error { return nil }
func (f *fakeWhatsApp) Send(_ context.Context, _ string, content string) error {
	f.mu.Lock()
	f.sent = append(f.sent, content)
	f.mu.Unlock()
	return nil
}
func (f *fakeWhatsApp) all() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.sent...)
}

func criticalRuntime(t *testing.T, p policy.Policy) (*KarmaxRuntime, *fakeWhatsApp) {
	t.Helper()
	db, err := store.New(filepath.Join(t.TempDir(), "t.db"), zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	b := bus.NewLog(db, "test", zap.NewNop())
	mgr := comms.NewManager(b, db, zap.NewNop())
	wa := &fakeWhatsApp{}
	if err := mgr.Register(wa, "nexus"); err != nil {
		t.Fatal(err)
	}
	return &KarmaxRuntime{store: db, log: zap.NewNop(), bus: b, comms: mgr, policy: p}, wa
}

func criticalEvent() bus.Event {
	return bus.NewEvent(bus.EventSystemCritical, "nexus", map[string]any{
		"severity": "critical", "message": "agent error streak reached restart threshold"})
}

// A critical alert is urgent by nature, so the default policy sends it — and
// it is always on record in the app, which it was not.
func TestACriticalAlertIsUrgent(t *testing.T) {
	rt, wa := criticalRuntime(t, policy.Default())
	if err := rt.raiseCritical(criticalEvent()); err != nil {
		t.Fatal(err)
	}
	if got := wa.all(); len(got) != 1 {
		t.Fatalf("WhatsApp got %v, want the alert", got)
	}
	if notes, _ := rt.store.ListNotifications(5); len(notes) != 1 {
		t.Errorf("the alert is not on record in the app: %+v", notes)
	}
}

// An operator who keeps urgent items off WhatsApp gets the alert in the app.
func TestACriticalAlertFollowsThePolicy(t *testing.T) {
	rt, wa := criticalRuntime(t, policy.New(policy.Config{WhatsApp: []string{"approvals"}}))
	if err := rt.raiseCritical(criticalEvent()); err != nil {
		t.Fatal(err)
	}
	if got := wa.all(); len(got) != 0 {
		t.Fatalf("WhatsApp got %v against the policy", got)
	}
	if notes, _ := rt.store.ListNotifications(5); len(notes) != 1 {
		t.Errorf("the alert is not on record in the app: %+v", notes)
	}
}
