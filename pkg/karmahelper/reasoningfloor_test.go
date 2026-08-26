package karmahelper

import "testing"

// A cap sized for the visible answer is spent entirely on thinking by a
// reasoning model, which returns an empty completion rather than an error.
// memory-review failed 204 times in a row on a 400-token judge.
func TestAReasoningModelGetsEnoughRoomToAnswer(t *testing.T) {
	for _, model := range []string{"gpt-5", "gpt-5-mini", "o3-mini", "claude-sonnet-4-6-thinking"} {
		if got := reasoningTokenFloor(model, 400); got < 2000 {
			t.Errorf("%s: got %d, a reasoning model needs room to think before it writes", model, got)
		}
	}
}

// A cap the caller chose deliberately, above the floor, is theirs to keep.
func TestAGenerousCapIsLeftAlone(t *testing.T) {
	if got := reasoningTokenFloor("gpt-5", 8192); got != 8192 {
		t.Errorf("got %d, want the caller's 8192 untouched", got)
	}
}

// Non-reasoning models spend the whole budget on output, so a small cap means
// exactly what it says and must not be inflated.
func TestNonReasoningModelsKeepTheirSmallCaps(t *testing.T) {
	for _, model := range []string{"claude-haiku-4-5-20251001-v1:0", "global.anthropic.claude-sonnet-4-6", "gpt-4o"} {
		if got := reasoningTokenFloor(model, 400); got != 400 {
			t.Errorf("%s: got %d, want 400 left alone", model, got)
		}
	}
}
