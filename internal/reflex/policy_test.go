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
	// mass is the probability on the chosen disposition. It defaults to
	// confidence when unset, but the two are different numbers and the
	// threshold is cut against this one.
	mass     float64
	urgency  float64
	risk     float64
	remember float64
	effort   string
}

func result(s sheet) *jev.Result {
	mass := s.mass
	if mass == 0 {
		mass = s.confidence
	}
	answers := jev.Answers{
		QDisposition: {
			Type:          jev.TypeChoice,
			Choice:        s.disposition,
			Confidence:    s.confidence,
			Probabilities: map[string]float64{s.disposition: mass, "handle": 1 - mass},
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

// The threshold is cut against the mass on the chosen option, not against the
// shape of the distribution. Across five options a clear winner still scores
// around 0.5 on shape, so cutting on confidence made drop almost unreachable
// while letting a 0.24 remember silence an event.
func TestSilentVerdictsAreCutOnMassNotConfidence(t *testing.T) {
	// Low shape-confidence, but nearly all the mass on drop: this is a drop.
	v := Decide(result(sheet{disposition: "drop", confidence: 0.40, mass: 0.88}), Hint{}, DefaultThresholds)
	if v.Action != ActionDrop {
		t.Errorf("action = %q, want drop when the mass is there", v.Action)
	}
	if v.Mass != 0.88 {
		t.Errorf("mass = %v, want it reported", v.Mass)
	}

	// High shape-confidence, but the mass is split: this is not.
	v = Decide(result(sheet{disposition: "drop", confidence: 0.95, mass: 0.50}), Hint{}, DefaultThresholds)
	if v.Action != ActionHandle {
		t.Errorf("action = %q, want handle when the mass is short", v.Action)
	}
}

// remember silences an event exactly as drop does, and additionally writes to
// the operator's memory, so it clears the same bar.
func TestUnsureRememberIsNotSilent(t *testing.T) {
	v := Decide(result(sheet{disposition: "remember", confidence: 0.24, mass: 0.24, remember: 0.9}),
		Hint{}, DefaultThresholds)
	if v.Silent() {
		t.Fatalf("a 0.24 remember must not silence the event: %+v", v)
	}
	if v.Action != ActionHandle {
		t.Errorf("action = %q, want handle", v.Action)
	}
}

func TestConfidentRememberStillFiles(t *testing.T) {
	v := Decide(result(sheet{disposition: "remember", confidence: 0.6, mass: 0.85, remember: 0.9}),
		Hint{}, DefaultThresholds)
	if v.Action != ActionRemember {
		t.Fatalf("action = %q, want remember when the mass is there", v.Action)
	}
}

// Acting verdicts are not silencing, so they are never held back by the bar —
// handling something on a weak signal is free, staying silent is not.
func TestActingVerdictsAreNotGatedOnMass(t *testing.T) {
	for _, disposition := range []string{"handle", "delegate", "escalate"} {
		v := Decide(result(sheet{disposition: disposition, confidence: 0.2, mass: 0.2}), Hint{}, DefaultThresholds)
		if v.Action != Action(disposition) {
			t.Errorf("%s was downgraded to %q on a low mass", disposition, v.Action)
		}
	}
}
