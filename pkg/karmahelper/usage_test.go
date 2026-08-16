package karmahelper

import (
	"strings"
	"sync"
	"testing"
)

// The meter exists because per-call-site accounting failed: three of twelve
// sessions recorded their spend, so the tracker reported a fifth of the bill.
// What matters is that a session cannot be created without being counted, and
// that an unlabelled one is visible rather than lost.
func TestEverySessionReportsItsSpend(t *testing.T) {
	var mu sync.Mutex
	var got []Usage
	OnUsage(func(u Usage) {
		mu.Lock()
		defer mu.Unlock()
		got = append(got, u)
	})
	defer OnUsage(nil)

	labelled := SessionConfig{Provider: "anthropic", Model: "m", Kind: "loop-gateway", AgentID: "nexus"}
	reportUsage(labelled, labelled.Provider, labelled.Model, TokenInfo{InputTokens: 100, OutputTokens: 20})
	reportUsage(SessionConfig{Provider: "anthropic", Model: "m"}, "anthropic", "m", TokenInfo{InputTokens: 5, OutputTokens: 1})
	// A call that produced nothing is not a call that cost nothing to report,
	// but a zero-token record is noise.
	reportUsage(labelled, "anthropic", "m", TokenInfo{})

	mu.Lock()
	defer mu.Unlock()
	if len(got) != 2 {
		t.Fatalf("recorded %d usages, want 2: %+v", len(got), got)
	}
	if got[0].Kind != "loop-gateway" || got[0].AgentID != "nexus" || got[0].InputTokens != 100 {
		t.Errorf("labelled usage = %+v", got[0])
	}
	if got[1].Kind != "unlabelled" {
		t.Errorf("an unlabelled session must still be counted, as unlabelled; got %q", got[1].Kind)
	}
}

func TestOversizedToolOutputIsCappedAndSaysSo(t *testing.T) {
	small := "a short result"
	if got := capToolOutput(small); got != small {
		t.Errorf("a small result must pass through unchanged, got %q", got)
	}
	big := capToolOutput(strings.Repeat("x", maxToolOutputChars*3))
	if len(big) >= maxToolOutputChars*3 {
		t.Fatalf("oversized result was not capped: %d chars", len(big))
	}
	// The model has to be able to tell truncation from the data ending.
	if !strings.Contains(big, "truncated") {
		t.Error("a capped result must say it was truncated")
	}
}
