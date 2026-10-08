package wasmloop

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"go.uber.org/zap"
)

// decidingKit captures what the host forwarded, which is the only way to see
// that a guest's questions survived the boundary intact.
type decidingKit struct {
	nullKit
	state     any
	questions []byte
	err       error
}

func (k *decidingKit) Decide(_ context.Context, state any, questions []byte) ([]byte, error) {
	k.state, k.questions = state, questions
	if k.err != nil {
		return nil, k.err
	}
	return []byte(`{"model":"jev-test","answers":{"reply":{"type":"noul","noul":0.91}}}`), nil
}

func newDecideRunner(kit Kit) *Runner {
	return &Runner{name: "decider", kit: kit, log: zap.NewNop()}
}

func TestDecidePassesStateAndQuestionsThrough(t *testing.T) {
	kit := &decidingKit{}
	r := newDecideRunner(kit)

	req := `{"state":{"message":"can you send the invoice?"},
	         "questions":{"reply":{"type":"noul","instructions":"Reply?"}}}`
	out, err := r.dispatch(context.Background(), FnDecide, req)
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}

	state, ok := kit.state.(map[string]any)
	if !ok {
		t.Fatalf("state reached the kit as %T, want a decoded object", kit.state)
	}
	if state["message"] != "can you send the invoice?" {
		t.Errorf("state = %v, want the guest's message", state)
	}

	// The questions must arrive as the guest wrote them: the host does not get
	// to reinterpret a loop's own question.
	var qs map[string]any
	if err := json.Unmarshal(kit.questions, &qs); err != nil {
		t.Fatalf("questions did not survive as JSON: %v", err)
	}
	if _, ok := qs["reply"]; !ok {
		t.Errorf("questions = %v, want the reply question", qs)
	}

	var decision struct {
		Model   string `json:"model"`
		Answers map[string]struct {
			Noul float64 `json:"noul"`
		} `json:"answers"`
	}
	if err := json.Unmarshal(out, &decision); err != nil {
		t.Fatalf("response: %v", err)
	}
	if decision.Answers["reply"].Noul != 0.91 {
		t.Errorf("noul = %v, want 0.91 back at the guest", decision.Answers["reply"].Noul)
	}
}

// A state is optional: a loop may judge its questions on their own.
func TestDecideAllowsAbsentState(t *testing.T) {
	kit := &decidingKit{}
	r := newDecideRunner(kit)
	if _, err := r.dispatch(context.Background(),
		FnDecide, `{"questions":{"q":{"type":"noul","instructions":"Yes?"}}}`); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if kit.state != nil {
		t.Errorf("state = %v, want nil when none was sent", kit.state)
	}
}

func TestDecideRejectsMalformedRequest(t *testing.T) {
	r := newDecideRunner(&decidingKit{})
	if _, err := r.dispatch(context.Background(), FnDecide, `not json`); err == nil {
		t.Fatal("expected an error for a malformed request")
	}
}

// Unavailability has to reach the guest as an error, never as a confident no.
func TestDecidePropagatesFailure(t *testing.T) {
	want := errors.New("reflex: unavailable")
	r := newDecideRunner(&decidingKit{err: want})
	_, err := r.dispatch(context.Background(),
		FnDecide, `{"questions":{"q":{"type":"noul","instructions":"Yes?"}}}`)
	if err == nil {
		t.Fatal("expected the failure to propagate")
	}
}

// The ABI is a closed set; a function without a description is refused.
func TestDecideIsDescribed(t *testing.T) {
	if _, ok := hostDescriptions[FnDecide]; !ok {
		t.Error("decide has no description, so a manifest listing it is refused")
	}
}
