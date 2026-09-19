package reflex

import (
	"testing"

	"github.com/MelloB1989/karma/ai/jev"
)

// result builds a Result the way the server would, so the policy is tested
// against the shape it actually receives.
type sheet struct {
	disposition string
	confidence  float64
	urgency     float64
	risk        float64
	remember    float64
	effort      string
}

func result(s sheet) *jev.Result {
	answers := jev.Answers{
		QDisposition: {
			Type:          jev.TypeChoice,
			Choice:        s.disposition,
			Confidence:    s.confidence,
			Probabilities: map[string]float64{s.disposition: s.confidence},
		},
		QUrgency:  {Type: jev.TypeScore, Score: s.urgency, Confidence: 0.9},
		QRisk:     {Type: jev.TypeScore, Score: s.risk, Confidence: 0.9},
		QRemember: {Type: jev.TypeNoul, Noul: s.remember},
	}
	if s.effort != "" {
		answers[QEffort] = jev.Answer{
			Type: jev.TypeChoice, Choice: s.effort, Confidence: 0.9,
			Probabilities: map[string]float64{s.effort: 0.9},
		}
	}
	return &jev.Result{Model: "jev-test", Answers: answers}
}

func TestConfidentDropIsDropped(t *testing.T) {
	v := Decide(result(sheet{disposition: "drop", confidence: 0.95}), Hint{}, DefaultThresholds)
	if v.Action != ActionDrop {
		t.Fatalf("action = %q, want drop", v.Action)
	}
	if v.FailedOpen {
		t.Fatal("a clean evaluation must not be marked failed-open")
	}
}

// An unsure drop is the failure mode that loses work, so it must not drop.
func TestUnsureDropIsHandled(t *testing.T) {
	v := Decide(result(sheet{disposition: "drop", confidence: 0.55}), Hint{}, DefaultThresholds)
	if v.Action != ActionHandle {
		t.Fatalf("action = %q, want handle for a low-confidence drop", v.Action)
	}
}

func TestDropCarryingAFactIsRemembered(t *testing.T) {
	v := Decide(result(sheet{disposition: "drop", confidence: 0.99, remember: 0.9}), Hint{}, DefaultThresholds)
	if v.Action != ActionRemember {
		t.Fatalf("action = %q, want remember", v.Action)
	}
}

// The operator floor is the promise that reflex chooses HOW, never whether.
func TestOperatorIsNeverSilenced(t *testing.T) {
	for _, disposition := range []string{"drop", "remember"} {
		v := Decide(result(sheet{disposition: disposition, confidence: 0.99, remember: 0.95}),
			Hint{Operator: true}, DefaultThresholds)
		if v.Silent() {
			t.Fatalf("operator event with disposition %q was silenced: %+v", disposition, v)
		}
		if !v.Floored {
			t.Fatalf("disposition %q should have been marked floored", disposition)
		}
	}
}

func TestNonOperatorIsNotFloored(t *testing.T) {
	v := Decide(result(sheet{disposition: "drop", confidence: 0.99}), Hint{Operator: false}, DefaultThresholds)
	if v.Action != ActionDrop {
		t.Fatalf("action = %q, want drop for a non-operator", v.Action)
	}
}

func TestHighUrgencyEscalates(t *testing.T) {
	// 3 of 4 levels is the top of the rubric, so urgency normalises to 1.
	v := Decide(result(sheet{disposition: "handle", confidence: 0.9, urgency: 3}), Hint{}, DefaultThresholds)
	if v.Action != ActionEscalate {
		t.Fatalf("action = %q, want escalate at full urgency", v.Action)
	}
}

func TestHighRiskRequiresApproval(t *testing.T) {
	v := Decide(result(sheet{disposition: "handle", confidence: 0.9, risk: 3}), Hint{}, DefaultThresholds)
	if !v.RequireApproval {
		t.Fatalf("risk %.2f should require approval", v.Risk)
	}
}

func TestLowRiskDoesNotRequireApproval(t *testing.T) {
	v := Decide(result(sheet{disposition: "handle", confidence: 0.9, risk: 0}), Hint{}, DefaultThresholds)
	if v.RequireApproval {
		t.Fatal("harmless events must not need approval")
	}
}

func TestEffortIsCarried(t *testing.T) {
	v := Decide(result(sheet{disposition: "delegate", confidence: 0.9, effort: "heavy"}), Hint{}, DefaultThresholds)
	if v.Effort != EffortHeavy {
		t.Fatalf("effort = %q, want heavy", v.Effort)
	}
	if v.Action != ActionDelegate {
		t.Fatalf("action = %q, want delegate", v.Action)
	}
}

// Anything unreadable must fail open rather than guess.
func TestUnknownDispositionFailsOpen(t *testing.T) {
	v := Decide(result(sheet{disposition: "explode", confidence: 0.99}), Hint{}, DefaultThresholds)
	if v.Action != ActionHandle || !v.FailedOpen {
		t.Fatalf("unknown disposition should fail open, got %+v", v)
	}
}

func TestNilResultFailsOpen(t *testing.T) {
	v := Decide(nil, Hint{}, DefaultThresholds)
	if v.Action != ActionHandle || !v.FailedOpen {
		t.Fatalf("nil result should fail open, got %+v", v)
	}
}

func TestMissingDispositionFailsOpen(t *testing.T) {
	v := Decide(&jev.Result{Answers: jev.Answers{}}, Hint{}, DefaultThresholds)
	if v.Action != ActionHandle || !v.FailedOpen {
		t.Fatalf("missing disposition should fail open, got %+v", v)
	}
}

func TestNormalise(t *testing.T) {
	for _, tc := range []struct {
		score  float64
		levels int
		want   float64
	}{
		{0, 4, 0}, {3, 4, 1}, {1.5, 4, 0.5}, {-1, 4, 0}, {9, 4, 1}, {1, 1, 0},
	} {
		if got := normalise(tc.score, tc.levels); got != tc.want {
			t.Errorf("normalise(%v, %d) = %v, want %v", tc.score, tc.levels, got, tc.want)
		}
	}
}

func TestThresholdDefaults(t *testing.T) {
	got := Thresholds{Drop: 0.8}.withDefaults()
	if got.Drop != 0.8 {
		t.Errorf("Drop = %v, want the override", got.Drop)
	}
	if got.Remember != DefaultThresholds.Remember {
		t.Errorf("Remember = %v, want the default", got.Remember)
	}
}
