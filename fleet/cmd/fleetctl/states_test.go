package main

import (
	"testing"

	"github.com/MelloB1989/karmax/fleet/internal/daemon"
)

// up --when-idle recreates an agent's container, and with it every slot:
// an agent whose second slot works, or holds an open task even between
// turns, is working, whatever its first slot does.
func TestAnAgentIsWorkingWhileAnyOfItsSlotsWorksOrHoldsATask(t *testing.T) {
	rows := []daemon.Row{
		{Agent: "agent-01", SlotOf: "agent-01", Slot: 1, State: "standby"},
		{Agent: "agent-01:2", SlotOf: "agent-01", Slot: 2, State: "working", Task: "t1"},
		{Agent: "agent-02", SlotOf: "agent-02", Slot: 1, State: "working", Task: "t2"},
		{Agent: "agent-02:2", SlotOf: "agent-02", Slot: 2, State: "standby"},
		{Agent: "agent-03", SlotOf: "agent-03", Slot: 1, State: "standby"},
		{Agent: "agent-03:2", SlotOf: "agent-03", Slot: 2, State: "waiting", Task: "t3"},
		{Agent: "agent-05", SlotOf: "agent-05", Slot: 1, State: "cooling"},
		{Agent: "agent-05:2", SlotOf: "agent-05", Slot: 2, State: "standby"},
		{Agent: "agent-04", State: "standby"}, // one slot, no SlotOf
	}
	got := agentStates(rows)
	want := map[string]string{"agent-01": "working", "agent-02": "working", "agent-03": "working", "agent-04": "standby", "agent-05": "cooling"}
	if len(got) != len(want) {
		t.Fatalf("states %v, want %v", got, want)
	}
	for a, s := range want {
		if got[a] != s {
			t.Errorf("%s: %q, want %q", a, got[a], s)
		}
	}
}
