package runtime

import (
	"path/filepath"
	"testing"

	"github.com/MelloB1989/karmax/internal/agent"
	"github.com/MelloB1989/karmax/internal/bus"
	"github.com/MelloB1989/karmax/internal/store"
	"go.uber.org/zap"
)

func routerRuntime(t *testing.T, claims func(string, map[string]any) bool) *KarmaxRuntime {
	t.Helper()
	db, err := store.New(filepath.Join(t.TempDir(), "t.db"), zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	reg := agent.NewRegistry(nil, db, zap.NewNop())
	// Registered but never started: a message that reaches it fails in Send,
	// which is observable and needs no model.
	if _, err := reg.Register(agent.AgentDef{ID: "nexus"}, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	return &KarmaxRuntime{store: db, log: zap.NewNop(), agents: reg, officeClaims: claims}
}

func chatEvent(content string) bus.Event {
	return bus.NewEvent(bus.EventCommsMessage, "nexus", map[string]any{
		"karmax_channel_id": "gchat", "channel_type": "gchat", "content": content, "direction": "inbound",
	})
}

func reachedAgent(rt *KarmaxRuntime, evt bus.Event) bool {
	turns, _ := rt.store.RecentTurns("nexus", 10)
	for _, tr := range turns {
		if tr.EventID == evt.ID {
			return true
		}
	}
	return false
}

// A message the office claims never reaches the agent; anything else still does.
func TestOfficeClaimedMessageSkipsTheAgent(t *testing.T) {
	claims := func(_ string, p map[string]any) bool { return p["content"] == "approve Q4HYX" }
	rt := routerRuntime(t, claims)

	approve := chatEvent("approve Q4HYX")
	if err := rt.routeToAgent(approve); err != nil {
		t.Fatalf("claimed message: %v", err)
	}
	if reachedAgent(rt, approve) {
		t.Error("an office-claimed approve was also delivered to the agent")
	}

	question := chatEvent("What does payments-api depend on?")
	_ = rt.routeToAgent(question) // the agent is not started, so Send errs; the turn row proves delivery
	if !reachedAgent(rt, question) {
		t.Error("a free-form question did not reach the agent")
	}
}

// With org_ops off nothing is claimed and delivery is unchanged.
func TestNoOfficeMeansEverythingReachesTheAgent(t *testing.T) {
	rt := routerRuntime(t, nil)
	evt := chatEvent("approve Q4HYX")
	_ = rt.routeToAgent(evt)
	if !reachedAgent(rt, evt) {
		t.Error("with no office, a message did not reach the agent")
	}
}
