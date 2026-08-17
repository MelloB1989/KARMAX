package review

import "testing"

// The three questions the operator actually received about one commitment,
// across two days, each from a different memory row. The exact-key latch let
// all three through because the rows differed; overlap has to catch them.
func TestTheSameQuestionInDifferentWordsIsRecognised(t *testing.T) {
	asked := []string{
		"Shravan Kumar podcast scripting — you promised this on Jul 12, re-promised Jul 29. Still happening, or should you loop back?",
		"Shravan Kumar's podcast/video scripting — promised since Jul 12, re-promised Jul 29. Still happening or should you drop it?",
		"Shravan's scripting/planning work — you committed to this on Jul 12 and again on Jul 29 ('soon, promise'). It's now 5 days overdue.",
	}
	for i := 0; i < len(asked); i++ {
		for j := i + 1; j < len(asked); j++ {
			if !sameQuestion(asked[i], asked[j]) {
				t.Errorf("questions %d and %d are the same question and were not recognised", i, j)
			}
		}
	}
	// Genuinely different questions must still get through.
	for _, other := range []string{
		"Did you carry the pen drive for Shiva's PC setup before the CampX meet?",
		"CampX APK and credentials were promised 2.5 weeks ago — resolved or still blocked?",
		"Did you send that offer letter to Kartik for Dr A Mallik Arjun Reddy?",
	} {
		for i, q := range asked {
			if sameQuestion(q, other) {
				t.Errorf("unrelated question was suppressed against question %d:\n  %s\n  %s", i, q, other)
			}
		}
	}
	// Sharing only a name is not sharing a subject.
	if sameQuestion("Did you send Shiva the APK build?", "Did you set up Shiva's laptop yet?") {
		t.Error("two different questions about the same person must both be asked")
	}
}

// The scaffolding every review question shares must not make two of them look
// alike on its own.
func TestBoilerplateAloneIsNotSimilarity(t *testing.T) {
	a := "Still pending or should you drop it? Reply: Done / Still working on it"
	b := "Still pending or should you drop it? Reply: Done / Drop it"
	if len(significantWords(a)) > 3 || len(significantWords(b)) > 3 {
		t.Errorf("boilerplate should reduce to almost nothing, got %v and %v",
			significantWords(a), significantWords(b))
	}
}
