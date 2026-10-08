package reflex

import (
	"strings"
	"testing"

	"github.com/MelloB1989/karma/ai/jev"
)

func TestParseQuestionsRoundTripsEveryType(t *testing.T) {
	qs, err := ParseQuestions([]byte(`{
		"reply":  {"type":"noul","instructions":"Should this be answered?",
		           "criteria":{"true":"It asks something","false":"It is small talk"}},
		"lane":   {"type":"choice","instructions":"Which lane?",
		           "criteria":{"urgent":"needs an answer today","later":null}},
		"heat":   {"type":"score","instructions":"How heated?",
		           "criteria":["calm","annoyed","furious"]}
	}`))
	if err != nil {
		t.Fatalf("parse: %v", err)
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

	// The criteria have to survive, not just the types.
	if n := len(qs["lane"].(jev.ChoiceQuestion).Criteria); n != 2 {
		t.Errorf("lane kept %d options, want 2", n)
	}
	if n := len(qs["heat"].(jev.ScoreQuestion).Criteria); n != 3 {
		t.Errorf("heat kept %d levels, want 3", n)
	}
	if qs["reply"].(jev.NoulQuestion).Criteria == nil {
		t.Error("reply lost its poles")
	}
}

func TestParseNoulWithoutCriteria(t *testing.T) {
	qs, err := ParseQuestions([]byte(`{"q":{"type":"noul","instructions":"Yes?"}}`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if qs["q"].(jev.NoulQuestion).Criteria != nil {
		t.Error("absent criteria should stay absent")
	}
}

func TestParseRejectsBadInput(t *testing.T) {
	for name, raw := range map[string]string{
		"not an object":  `[]`,
		"empty":          `{}`,
		"no type":        `{"q":{"instructions":"Yes?"}}`,
		"unknown type":   `{"q":{"type":"vibes","instructions":"Yes?"}}`,
		"choice as list": `{"q":{"type":"choice","instructions":"Which?","criteria":["a","b"]}}`,
		"score as map":   `{"q":{"type":"score","instructions":"How?","criteria":{"a":"b"}}}`,
	} {
		if _, err := ParseQuestions([]byte(raw)); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

// The id has to be in the message, or a loop author has nothing to debug with.
func TestParseErrorNamesTheQuestion(t *testing.T) {
	_, err := ParseQuestions([]byte(`{"my_question":{"type":"vibes","instructions":"Yes?"}}`))
	if err == nil || !strings.Contains(err.Error(), "my_question") {
		t.Fatalf("error should name the question, got %v", err)
	}
}

func TestParseBoundsQuestionCount(t *testing.T) {
	var b strings.Builder
	b.WriteString("{")
	for i := 0; i < maxQuestions+1; i++ {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(`"q`)
		b.WriteString(string(rune('a' + i%26)))
		b.WriteString(string(rune('a' + i/26)))
		b.WriteString(`":{"type":"noul","instructions":"Yes?"}`)
	}
	b.WriteString("}")
	if _, err := ParseQuestions([]byte(b.String())); err == nil {
		t.Fatal("expected a limit error")
	}
}

// Instructions may be a JSON object when a question has parts worth naming.
func TestParseStructuredInstructions(t *testing.T) {
	qs, err := ParseQuestions([]byte(`{"q":{"type":"noul","instructions":{"ask":"Reply?","tone":"terse"}}}`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if _, ok := qs["q"].(jev.NoulQuestion).Instructions.(map[string]any); !ok {
		t.Errorf("structured instructions were flattened: %T", qs["q"].(jev.NoulQuestion).Instructions)
	}
}
