package memory

import (
	"math"
	"testing"
	"time"
)

func TestRelevanceBoost_ImportanceOrdering(t *testing.T) {
	low := relevanceBoost(MemoryEntry{Importance: 1})
	med := relevanceBoost(MemoryEntry{Importance: 2})
	high := relevanceBoost(MemoryEntry{Importance: 3})
	crit := relevanceBoost(MemoryEntry{Importance: 4})
	if !(low < med && med < high && high < crit) {
		t.Errorf("boost must increase with importance: low=%f med=%f high=%f crit=%f", low, med, high, crit)
	}
}

func TestRelevanceBoost_PinnedRecentDominatesStaleLow(t *testing.T) {
	staleLow := relevanceBoost(MemoryEntry{
		Importance: 1,
		CreatedAt:  time.Now().Add(-200 * 24 * time.Hour),
	})
	pinnedRecentCritical := relevanceBoost(MemoryEntry{
		Importance:  4,
		Pinned:      true,
		AccessCount: 5,
		CreatedAt:   time.Now(),
	})
	if pinnedRecentCritical <= staleLow {
		t.Errorf("pinned/critical/recent (%f) should rank above stale low-importance (%f)",
			pinnedRecentCritical, staleLow)
	}
}

func TestRelevanceBoost_RecencyDecays(t *testing.T) {
	fresh := relevanceBoost(MemoryEntry{Importance: 2, CreatedAt: time.Now()})
	old := relevanceBoost(MemoryEntry{Importance: 2, CreatedAt: time.Now().Add(-120 * 24 * time.Hour)})
	if fresh <= old {
		t.Errorf("fresh memory (%f) should outrank old memory (%f)", fresh, old)
	}
}

func TestJaccardSimilarity_ExactMatch(t *testing.T) {
	sim := jaccardSimilarity("hello world foo bar", "hello world foo bar")
	if math.Abs(sim-1.0) > 0.001 {
		t.Errorf("exact match should be 1.0, got %f", sim)
	}
}

func TestJaccardSimilarity_NoOverlap(t *testing.T) {
	sim := jaccardSimilarity("alpha beta gamma", "delta epsilon zeta")
	if sim != 0.0 {
		t.Errorf("no overlap should be 0.0, got %f", sim)
	}
}

func TestJaccardSimilarity_PartialOverlap(t *testing.T) {
	// Words: {a, b, c} vs {b, c, d} => intersection={b,c}=2, union={a,b,c,d}=4
	sim := jaccardSimilarity("a b c", "b c d")
	expected := 2.0 / 4.0
	if math.Abs(sim-expected) > 0.001 {
		t.Errorf("expected %f, got %f", expected, sim)
	}
}

func TestJaccardSimilarity_EmptyStrings(t *testing.T) {
	if sim := jaccardSimilarity("", "hello"); sim != 0.0 {
		t.Errorf("empty first string should return 0.0, got %f", sim)
	}
	if sim := jaccardSimilarity("hello", ""); sim != 0.0 {
		t.Errorf("empty second string should return 0.0, got %f", sim)
	}
	if sim := jaccardSimilarity("", ""); sim != 0.0 {
		t.Errorf("both empty should return 0.0, got %f", sim)
	}
}

func TestExtractKeywords(t *testing.T) {
	keywords := extractKeywords("The quick brown fox jumps over the lazy dog")
	// "the" and "a" are stopwords, should be removed
	for _, kw := range keywords {
		if kw == "the" {
			t.Error("stopword 'the' should be removed")
		}
	}

	// "quick", "brown", "fox" should be present
	found := make(map[string]bool)
	for _, kw := range keywords {
		found[kw] = true
	}
	for _, expected := range []string{"quick", "brown", "fox", "jumps", "over", "lazy", "dog"} {
		if !found[expected] {
			t.Errorf("expected keyword %q not found in result", expected)
		}
	}
}

func TestExtractKeywords_AllStopwords(t *testing.T) {
	keywords := extractKeywords("the a an is are")
	if len(keywords) != 0 {
		t.Errorf("all stopwords should yield empty result, got %v", keywords)
	}
}

func TestExtractKeywords_ShortTokens(t *testing.T) {
	keywords := extractKeywords("I a x go")
	// "I" (len 1), "a" (stopword), "x" (len 1) should be removed
	// "go" (len 2) should be kept
	found := make(map[string]bool)
	for _, kw := range keywords {
		found[kw] = true
	}
	if !found["go"] {
		t.Error("'go' should be kept (len >= 2)")
	}
	if found["i"] || found["x"] {
		t.Error("single-char tokens should be dropped")
	}
}

func TestSplitIntoChunks_Paragraphs(t *testing.T) {
	text := "First paragraph about Go.\n\nSecond paragraph about testing.\n\nThird paragraph about deployment."
	chunks := splitIntoChunks(text)
	if len(chunks) != 3 {
		t.Errorf("expected 3 chunks from paragraphs, got %d", len(chunks))
	}
}

func TestSplitIntoChunks_Sentences(t *testing.T) {
	text := "First sentence about Go. Second sentence about testing. Third sentence about deployment."
	chunks := splitIntoChunks(text)
	if len(chunks) < 2 {
		t.Errorf("expected at least 2 chunks from sentences, got %d", len(chunks))
	}
}

func TestSplitIntoChunks_SingleChunk(t *testing.T) {
	text := "Just a single block of text with no paragraph breaks or sentence endings"
	chunks := splitIntoChunks(text)
	if len(chunks) != 1 {
		t.Errorf("expected 1 chunk, got %d", len(chunks))
	}
	if chunks[0] != text {
		t.Errorf("single chunk should be the full text")
	}
}

func TestMakeWordSet(t *testing.T) {
	set := makeWordSet("hello, world! hello again.")
	if !set["hello"] {
		t.Error("expected 'hello' in set")
	}
	if !set["world"] {
		t.Error("expected 'world' in set (punctuation stripped)")
	}
	if !set["again"] {
		t.Error("expected 'again' in set")
	}
	if set["world!"] {
		t.Error("'world!' with punctuation should not be in set")
	}
}

func TestTruncate(t *testing.T) {
	if got := truncate("short", 10); got != "short" {
		t.Errorf("expected 'short', got %q", got)
	}
	if got := truncate("hello world foo", 5); got != "hello..." {
		t.Errorf("expected 'hello...', got %q", got)
	}
	if got := truncate("", 5); got != "" {
		t.Errorf("expected empty, got %q", got)
	}
}
