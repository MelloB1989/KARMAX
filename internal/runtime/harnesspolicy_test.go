package runtime

import (
	"slices"
	"testing"
	"time"

	"github.com/MelloB1989/karmax/internal/config"
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
    launch: [docker, exec, -i, -w, "{workdir}", karmax-brain]
  chat:
    model: sonnet
`
	if err := yaml.Unmarshal([]byte(src), &hc); err != nil {
		t.Fatal(err)
	}
	pols := harnessPolicies(hc)
	agent := pols["agent"]
	if agent.Name != "karmax" || agent.Idle != 30*time.Minute {
		t.Errorf("agent policy = %+v", agent)
	}
	if !slices.Equal(agent.Launch, []string{"docker", "exec", "-i", "-w", "{workdir}", "karmax-brain"}) {
		t.Errorf("agent launch = %q", agent.Launch)
	}
	// A kind that says nothing about the fleet runs exactly as before.
	if chat := pols["chat"]; chat.Name != "" || chat.Launch != nil {
		t.Errorf("chat picked up fleet settings it never asked for: %+v", chat)
	}
}
