package runtime

import (
	"slices"
	"testing"
	"time"

	"github.com/MelloB1989/karmax/internal/bus"
	"github.com/MelloB1989/karmax/internal/config"
	"github.com/MelloB1989/karmax/internal/harness"
	"gopkg.in/yaml.v3"
)

// The fleet's daemon settings are read from karmax.yaml and reach the
// supervisor's policy unchanged.
func TestHarnessPoliciesCarryTheFleetSettings(t *testing.T) {
	var hc config.HarnessConfig
	src := `
kinds:
  agent:
    model: sonnet
    idle: 30m
    name: karmax
    resident: true
    launch: [docker, exec, -i, -w, "{workdir}", karmax-brain]
  chat:
    model: sonnet
`
	if err := yaml.Unmarshal([]byte(src), &hc); err != nil {
		t.Fatal(err)
	}
	pols := harnessPolicies(hc)
	agent := pols["agent"]
	if agent.Name != "karmax" || agent.Idle != 30*time.Minute || !agent.Resident {
		t.Errorf("agent policy = %+v", agent)
	}
	if !slices.Equal(agent.Launch, []string{"docker", "exec", "-i", "-w", "{workdir}", "karmax-brain"}) {
		t.Errorf("agent launch = %q", agent.Launch)
	}
	// A kind that says nothing about the fleet runs exactly as before.
	if chat := pols["chat"]; chat.Name != "" || chat.Launch != nil || chat.Resident {
		t.Errorf("chat picked up fleet settings it never asked for: %+v", chat)
	}
}

// A turn the orchestrator ran by itself — an agent's report arriving while it
// was idle — reaches the bus with what it said, what it did and what it cost.
func TestBackgroundTurnBecomesABusEvent(t *testing.T) {
	ev := backgroundTurnEvent("agent:main", "agent", harness.Turn{
		Text:      "agent-03 finished the fix",
		ToolCalls: []harness.ToolCall{{Name: "Bash"}, {Name: "SendMessage"}},
		CostUSD:   0.12,
		Model:     "claude-sonnet",
	})
	if ev.Kind != bus.EventHarnessBackgroundTurn || string(ev.Kind) != "harness.turn.background" {
		t.Fatalf("kind = %q", ev.Kind)
	}
	p := ev.Payload
	if p["session"] != "agent:main" || p["kind"] != "agent" || p["text"] != "agent-03 finished the fix" ||
		p["cost_usd"] != 0.12 || p["model"] != "claude-sonnet" {
		t.Errorf("payload = %v", p)
	}
	if tools, _ := p["tools"].([]string); !slices.Equal(tools, []string{"Bash", "SendMessage"}) {
		t.Errorf("tools = %v", p["tools"])
	}
	if _, has := p["error"]; has {
		t.Error("a turn that succeeded carries no error")
	}
}

// Each agent thinks in a session of its own harness kind — "agent" unless it
// names another — so one agent can be the fleet's orchestrator (launched,
// named, resident) without making every agent on the host one.
func TestAnAgentChoosesItsHarnessKind(t *testing.T) {
	if got := harnessKindOf(configToAgentDef(config.AgentDefConfig{ID: "a"})); got != "agent" {
		t.Errorf("default kind = %q, want agent", got)
	}
	if got := harnessKindOf(configToAgentDef(config.AgentDefConfig{ID: "b", HarnessKind: "orchestrator"})); got != "orchestrator" {
		t.Errorf("kind = %q, want orchestrator", got)
	}
}
