package integrations

import (
	"testing"

	"github.com/MelloB1989/karmax/internal/config"
)

func TestSupportedChannelTypesCoversWhatIsImplemented(t *testing.T) {
	// A channel type the runtime starts is listed even before one is
	// configured; otherwise it is invisible until somebody guesses its name.
	got := map[string]bool{}
	for _, n := range SupportedChannelTypes() {
		got[n] = true
	}
	for _, want := range []string{"slack"} {
		if !got[want] {
			t.Errorf("%s is implemented but not listed as supported", want)
		}
	}
}

func TestUnconfiguredChannelTypesExcludesWhatIsConfigured(t *testing.T) {
	cfg := &config.KarmaxConfig{Comms: config.CommsConfig{Channels: []config.ChannelConfig{
		{ID: "slack-main", Type: "Slack"}, // case must not matter
	}}}
	for _, n := range UnconfiguredChannelTypes(cfg) {
		if n == "slack" {
			t.Error("slack is configured and must not be reported as missing")
		}
	}
}

func TestUnconfiguredChannelTypesListsEverythingWhenNothingIsConfigured(t *testing.T) {
	got := UnconfiguredChannelTypes(&config.KarmaxConfig{})
	if len(got) != len(SupportedChannelTypes()) {
		t.Errorf("with no channels configured all should be listed, got %v", got)
	}
}

func TestUnconfiguredChannelTypesToleratesNoConfig(t *testing.T) {
	if got := UnconfiguredChannelTypes(nil); len(got) != len(SupportedChannelTypes()) {
		t.Errorf("a nil config should not panic or hide anything, got %v", got)
	}
}
