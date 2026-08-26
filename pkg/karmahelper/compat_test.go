package karmahelper

import "testing"

// One predicate decides both the temperature guard and the token floor. They
// were written separately and had already drifted — this one knew gpt-5 and
// "thinking" but not o1/o3, so an o-series model got a temperature (a 400 on
// every call) while the floor beside it treated the same model as reasoning.
func TestOnePredicateDecidesBothGuards(t *testing.T) {
	reasoning := []string{
		"gpt-5", "gpt-5-mini", "GPT-5", "o1-preview", "o3-mini",
		"claude-sonnet-4-6-thinking",
	}
	for _, m := range reasoning {
		if !isReasoningModel(m) {
			t.Errorf("%s should be treated as a reasoning model", m)
		}
		// Both guards must agree: no temperature, and room to think.
		if got := reasoningTokenFloor(m, 400); got == 400 {
			t.Errorf("%s: the token floor disagrees with the temperature guard", m)
		}
	}

	conventional := []string{
		"gpt-4o", "gpt-4o-mini", "global.anthropic.claude-sonnet-4-6",
		"claude-haiku-4-5-20251001-v1:0", "deepseek-3.2",
	}
	for _, m := range conventional {
		if isReasoningModel(m) {
			t.Errorf("%s is not a reasoning model and must keep its tuned sampling", m)
		}
		if got := reasoningTokenFloor(m, 400); got != 400 {
			t.Errorf("%s: a deliberate small cap must survive, got %d", m, got)
		}
	}
}

// The guard reads the configured model string, which in production is a
// provider-prefixed inference profile, not a bare name.
func TestGuardsSeeThroughProviderPrefixes(t *testing.T) {
	if !isReasoningModel("azure/deployments/gpt-5-chat") {
		t.Error("a deployment path containing gpt-5 must still be recognised")
	}
	if isReasoningModel("") {
		t.Error("an unset model must not be guessed at")
	}
}
