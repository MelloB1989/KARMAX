package reflex

import (
	"encoding/json"
	"testing"

	"github.com/MelloB1989/karma/ai/jev"
	"github.com/MelloB1989/karmax/pkg/loopkit"
	"github.com/MelloB1989/karmax/pkg/loopwasm"
)

// The seam between the SDKs a loop author writes against and the decoder that
// rebuilds their questions here.
//
// These are three separate declarations of the same wire shape — loopkit for a
// compiled-in loop, loopwasm for a sandboxed one, and the wireQuestion decoder
// for both — so nothing but a test stops one of them drifting. Drift would not
// fail a build: it would fail at runtime, in a loop, as a judgement that never
// arrives.

func TestLoopkitQuestionsDecode(t *testing.T) {
	encoded, err := json.Marshal(loopkit.Questions{
		"reply": loopkit.Noul("Does this need a reply?").
			When("It asks something", "Small talk"),
		"lane": loopkit.Choice("Who handles it?", map[string]string{
			"assistant": "answerable from what is known",
			"operator":  "",
		}),
		"heat": loopkit.Score("How heated?", "calm", "annoyed", "furious"),
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	assertDecodes(t, encoded)
}

func TestLoopwasmQuestionsDecode(t *testing.T) {
	encoded, err := json.Marshal(loopwasm.Questions{
		"reply": loopwasm.Noul("Does this need a reply?").
			When("It asks something", "Small talk"),
		"lane": loopwasm.Choice("Who handles it?", map[string]string{
			"assistant": "answerable from what is known",
			"operator":  "",
		}),
		"heat": loopwasm.Score("How heated?", "calm", "annoyed", "furious"),
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	assertDecodes(t, encoded)
}

func assertDecodes(t *testing.T, encoded []byte) {
	t.Helper()
	qs, err := ParseQuestions(encoded)
	if err != nil {
		t.Fatalf("the host could not rebuild the SDK's questions: %v", err)
	}
	if len(qs) != 3 {
		t.Fatalf("got %d questions, want 3", len(qs))
	}
	if qs["reply"].Type() != jev.TypeNoul {
		t.Errorf("reply is %q", qs["reply"].Type())
	}
	if qs["lane"].Type() != jev.TypeChoice {
		t.Errorf("lane is %q", qs["lane"].Type())
	}
	if qs["heat"].Type() != jev.TypeScore {
		t.Errorf("heat is %q", qs["heat"].Type())
	}

	// An option the author left undescribed must survive as an option, not be
	// dropped for having no description.
	options := qs["lane"].(jev.ChoiceQuestion).Criteria
	if _, ok := options["operator"]; !ok {
		t.Errorf("an undescribed option was lost: %v", options)
	}
	if len(qs["heat"].(jev.ScoreQuestion).Criteria) != 3 {
		t.Error("score levels did not survive")
	}
	if qs["reply"].(jev.NoulQuestion).Criteria == nil {
		t.Error("noul poles did not survive")
	}
}

// The two answer shapes are read by loop authors, so their field names have to
// match what the host actually sends.
func TestAnswerShapesAgree(t *testing.T) {
	encoded, err := json.Marshal(jev.Answer{
		Type: jev.TypeChoice, Choice: "assistant", Confidence: 0.8,
		Probabilities: map[string]float64{"assistant": 0.8, "operator": 0.2},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var kitAnswer loopkit.Answer
	if err := json.Unmarshal(encoded, &kitAnswer); err != nil {
		t.Fatalf("loopkit: %v", err)
	}
	var wasmAnswer loopwasm.Answer
	if err := json.Unmarshal(encoded, &wasmAnswer); err != nil {
		t.Fatalf("loopwasm: %v", err)
	}
	if kitAnswer.Choice != "assistant" || wasmAnswer.Choice != "assistant" {
		t.Errorf("choice did not survive: %q / %q", kitAnswer.Choice, wasmAnswer.Choice)
	}
	if kitAnswer.Confidence != 0.8 || wasmAnswer.Confidence != 0.8 {
		t.Errorf("confidence did not survive: %v / %v", kitAnswer.Confidence, wasmAnswer.Confidence)
	}
	if len(wasmAnswer.Probabilities) != 2 {
		t.Errorf("probabilities did not survive: %v", wasmAnswer.Probabilities)
	}
}
