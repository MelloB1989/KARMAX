package builtin

import (
	"math"
	"testing"
)

func TestWordOverlap_ExactMatch(t *testing.T) {
	sim := wordOverlap("the quick brown fox jumps over the lazy dog", "the quick brown fox jumps over the lazy dog")
	if math.Abs(sim-1.0) > 0.001 {
		t.Errorf("exact match should be 1.0, got %f", sim)
	}
}

func TestWordOverlap_NoOverlap(t *testing.T) {
	sim := wordOverlap("alpha beta gamma", "delta epsilon zeta")
	if sim != 0.0 {
		t.Errorf("no overlap should be 0.0, got %f", sim)
	}
}

func TestWordOverlap_PartialOverlap(t *testing.T) {
	sim := wordOverlap("the quick brown fox", "the slow brown cat")
	if sim <= 0.0 || sim >= 1.0 {
		t.Errorf("partial overlap should be between 0 and 1, got %f", sim)
	}
	// "the" and "brown" overlap out of {"the", "quick", "brown", "fox", "slow", "cat"} = 6 unique
	// Jaccard = 2/6 = 0.333...
	expected := 2.0 / 6.0
	if math.Abs(sim-expected) > 0.01 {
		t.Errorf("expected ~%f, got %f", expected, sim)
	}
}

func TestWordOverlap_EmptyStrings(t *testing.T) {
	if sim := wordOverlap("", "hello"); sim != 0.0 {
		t.Errorf("empty first string should return 0.0, got %f", sim)
	}
	if sim := wordOverlap("hello", ""); sim != 0.0 {
		t.Errorf("empty second string should return 0.0, got %f", sim)
	}
	if sim := wordOverlap("", ""); sim != 0.0 {
		t.Errorf("both empty should return 0.0, got %f", sim)
	}
}

func TestWordOverlap_CaseInsensitive(t *testing.T) {
	sim := wordOverlap("Hello World", "hello world")
	if math.Abs(sim-1.0) > 0.001 {
		t.Errorf("case insensitive match should be 1.0, got %f", sim)
	}
}

func TestWordOverlap_HighSimilarity(t *testing.T) {
	a := "Kartik prefers Go for backend services with PostgreSQL and Redis"
	b := "Kartik prefers Go for backend with PostgreSQL and Redis caching"
	sim := wordOverlap(a, b)
	if sim < 0.7 {
		t.Errorf("high similarity strings should have overlap > 0.7, got %f", sim)
	}
}

func TestWordSet(t *testing.T) {
	set := wordSet("hello, world! hello again.")
	if !set["hello"] {
		t.Error("expected 'hello' in word set")
	}
	if !set["world"] {
		t.Error("expected 'world' in word set")
	}
	if !set["again"] {
		t.Error("expected 'again' in word set")
	}
	// Punctuation should be stripped
	if set["world!"] {
		t.Error("punctuation should be stripped from 'world!'")
	}
}
