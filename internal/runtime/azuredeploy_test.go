package runtime

import (
	"testing"

	"github.com/MelloB1989/karma/ai"
	"github.com/MelloB1989/karmax/internal/config"
	"go.uber.org/zap"
)

// The deployment name used to be compiled in, so a name chosen for ONE Azure
// resource shipped to everybody: gpt-5-mini was pinned to "karmax-gpt-5-mini"
// and 404'd with DeploymentNotFound anywhere else.
func TestAModelDefaultsToADeploymentOfTheSameName(t *testing.T) {
	m := azureDeployments(config.ProviderConfig{}, zap.NewNop())

	if got := m[ai.GPT5Mini]; got != "gpt-5-mini" {
		t.Errorf("gpt-5-mini should default to its own name, got %q", got)
	}
	if got := m[ai.GPT5]; got != "gpt-5" {
		t.Errorf("gpt-5 should default to its own name, got %q", got)
	}
}

func TestConfigOverridesADeploymentName(t *testing.T) {
	m := azureDeployments(config.ProviderConfig{
		Deployments: map[string]string{"gpt-5-mini": "karmax-gpt-5-mini"},
	}, zap.NewNop())

	if got := m[ai.GPT5Mini]; got != "karmax-gpt-5-mini" {
		t.Errorf("config should win, got %q", got)
	}
	// Overriding one model must not disturb the other.
	if got := m[ai.GPT5]; got != "gpt-5" {
		t.Errorf("untouched model changed, got %q", got)
	}
}

// A model KARMAX has no constant for still has to be addressable — resolveModel
// passes unknown names through, so a deployment can be named for anything.
func TestAnUnknownModelCanBeMapped(t *testing.T) {
	m := azureDeployments(config.ProviderConfig{
		Deployments: map[string]string{"gpt-5.4": "gpt-5.4"},
	}, zap.NewNop())

	if got := m[ai.BaseModel("gpt-5.4")]; got != "gpt-5.4" {
		t.Errorf("unknown model was not mapped, got %q", got)
	}
}

// Half a mapping is a config typo; dropping it keeps the default rather than
// registering a request for a deployment named "".
func TestEmptyMappingsAreIgnored(t *testing.T) {
	m := azureDeployments(config.ProviderConfig{
		Deployments: map[string]string{"gpt-5-mini": "", "": "orphan"},
	}, zap.NewNop())

	if got := m[ai.GPT5Mini]; got != "gpt-5-mini" {
		t.Errorf("empty deployment should leave the default, got %q", got)
	}
	if got := m[ai.BaseModel("")]; got != "" {
		t.Errorf("empty model name should not be registered, got %q", got)
	}
}
