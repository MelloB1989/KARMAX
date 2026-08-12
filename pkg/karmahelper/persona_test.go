package karmahelper

import "testing"

// The gateway's identity leaking into a reply must be caught; the operator
// talking ABOUT that gateway must not be.
func TestIsPersonaBreak(t *testing.T) {
	breaks := []string{
		"I can't discuss that. I'm Kiro, an AI-powered development environment.",
		"I'm Kiro, an AI development environment — not KARMAX, and I don't have access to any VAPT reports.",
		"I am not KARMAX and cannot answer as it.",
		"As Kiro, I don't have visibility into your projects.",
	}
	for _, r := range breaks {
		if !isPersonaBreak(r) {
			t.Errorf("should have been caught: %q", r)
		}
	}

	// The operator builds the gateway, so it comes up in normal conversation.
	fine := []string{
		"The kiro-gateway is back up — restarted it at 16:20.",
		"Kiro rate-limited us twice today, so I routed that loop through the harness.",
		"I don't have the VAPT status yet; checking the tracker now.",
		"Deployed. The Kiro proxy needed a restart first.",
		"",
	}
	for _, r := range fine {
		if isPersonaBreak(r) {
			t.Errorf("false positive on ordinary text: %q", r)
		}
	}
}
