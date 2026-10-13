package runtime

import (
	"strings"
	"testing"

	"github.com/MelloB1989/karmax/internal/agent"
)

func TestHarnessAgentPromptCarriesConfiguredPrompt(t *testing.T) {
	got := harnessAgentPrompt(agent.AgentDef{SystemPrompt: "You are the engineering agent of the acme organisation.\n"})
	for _, want := range []string{"engineering agent of the acme", "THIS section wins", "karmax tool call"} {
		if !strings.Contains(got, want) {
			t.Errorf("brief lacks %q:\n%s", want, got)
		}
	}
	if !strings.HasPrefix(got, "# Your role") {
		t.Error("precedence preamble must come before the configured prompt")
	}
}

func TestHarnessAgentPromptEmptyWhenUnset(t *testing.T) {
	for _, p := range []string{"", "  \n"} {
		if got := harnessAgentPrompt(agent.AgentDef{SystemPrompt: p}); got != "" {
			t.Errorf("prompt %q produced %q", p, got)
		}
	}
}

func TestHarnessAgentPromptTracksConfig(t *testing.T) {
	a := harnessAgentPrompt(agent.AgentDef{SystemPrompt: "one"})
	b := harnessAgentPrompt(agent.AgentDef{SystemPrompt: "two"})
	if a == b {
		t.Error("a changed system_prompt produced the same brief, so no respawn would happen")
	}
}
