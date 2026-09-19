package runtime

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/MelloB1989/karma/ai/jev"
	"github.com/MelloB1989/karmax/internal/reflex"
	"github.com/MelloB1989/karmax/pkg/loopkit"
)

// Reaching System One from a loop.
//
// A loop that wanted a judgement had only Kit.Ask, which spends a full model
// turn on it. Most of those questions are small — is this worth replying to,
// which lane does it belong in, how urgent is it — and a turn is the wrong
// instrument for a small question. Decide answers all of them in one pass,
// in well under a second, billed on input alone.

// Decide answers a loop's own typed questions.
func (k *loopKit) Decide(ctx context.Context, state any, questions loopkit.Questions) (*loopkit.Decision, error) {
	return k.rt.decide(ctx, state, questions)
}

// Decide is the wasm guest's route to the same place. The questions are the
// loop's, so they cross the sandbox as JSON and are rebuilt here.
func (w *wasmKit) Decide(ctx context.Context, state any, questionsJSON []byte) ([]byte, error) {
	qs, err := reflex.ParseQuestions(questionsJSON)
	if err != nil {
		return nil, err
	}
	res, err := w.rt.evaluate(ctx, state, qs)
	if err != nil {
		return nil, err
	}
	return json.Marshal(decisionOf(res))
}

// decide runs a loopkit question set and converts the answers back.
func (rt *KarmaxRuntime) decide(ctx context.Context, state any, questions loopkit.Questions) (*loopkit.Decision, error) {
	if len(questions) == 0 {
		return nil, fmt.Errorf("decide: no questions")
	}
	// Round-tripped through the wire form so the compiled-in path and the wasm
	// path are answering questions built the same way. One encoder means one
	// place for a question shape to be wrong.
	encoded, err := json.Marshal(questions)
	if err != nil {
		return nil, fmt.Errorf("decide: %w", err)
	}
	qs, err := reflex.ParseQuestions(encoded)
	if err != nil {
		return nil, err
	}
	res, err := rt.evaluate(ctx, state, qs)
	if err != nil {
		return nil, err
	}
	return decisionOf(res), nil
}

// evaluate is the single point where a loop's question reaches TypeSafe.
func (rt *KarmaxRuntime) evaluate(ctx context.Context, state any, qs jev.Questions) (*jev.Result, error) {
	if !rt.reflex.Available() {
		return nil, reflex.ErrUnavailable
	}
	return rt.reflex.Ask(ctx, state, qs)
}

// decisionOf converts a jev result into the loop-facing shape.
func decisionOf(res *jev.Result) *loopkit.Decision {
	out := &loopkit.Decision{
		Model:   res.Model,
		Answers: make(map[string]loopkit.Answer, len(res.Answers)),
	}
	for id, a := range res.Answers {
		out.Answers[id] = loopkit.Answer{
			Type:          loopkit.QuestionType(a.Type),
			Noul:          a.Noul,
			Choice:        a.Choice,
			Score:         a.Score,
			Probabilities: a.Probabilities,
			Confidence:    a.Confidence,
		}
	}
	return out
}
