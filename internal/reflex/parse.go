package reflex

import (
	"encoding/json"
	"fmt"

	"github.com/MelloB1989/karma/ai/jev"
)

// Decoding questions that arrived as JSON.
//
// A loop phrases its own questions — whether to reply, how to triage, which of
// its branches to take — so they cross the sandbox boundary as data and have to
// be rebuilt into typed questions here. jev marshals questions but does not
// unmarshal them, and it should not: the wire shape is this boundary's problem.

// maxQuestions bounds one request. Extra questions are nearly free to ask, but
// not free to build, and a loop that sends thousands is a loop with a bug.
const maxQuestions = 32

// wireQuestion is one question as a guest writes it.
type wireQuestion struct {
	Type         string          `json:"type"`
	Instructions json.RawMessage `json:"instructions"`
	// Criteria is a different shape per type: two named poles for a noul, an
	// option map for a choice, an ordered list for a score.
	Criteria json.RawMessage `json:"criteria"`
}

// ParseQuestions rebuilds typed questions from their wire form. Every failure
// names the question id, because a loop author reading the error has nothing
// else to go on.
func ParseQuestions(raw []byte) (jev.Questions, error) {
	var wire map[string]wireQuestion
	if err := json.Unmarshal(raw, &wire); err != nil {
		return nil, fmt.Errorf("reflex: questions are not an object: %w", err)
	}
	if len(wire) == 0 {
		return nil, fmt.Errorf("reflex: no questions")
	}
	if len(wire) > maxQuestions {
		return nil, fmt.Errorf("reflex: %d questions, at most %d", len(wire), maxQuestions)
	}

	out := make(jev.Questions, len(wire))
	for id, w := range wire {
		q, err := parseOne(id, w)
		if err != nil {
			return nil, err
		}
		out[id] = q
	}
	return out, nil
}

func parseOne(id string, w wireQuestion) (jev.Question, error) {
	instructions, err := decodeAny(w.Instructions)
	if err != nil {
		return nil, fmt.Errorf("reflex: question %q has unreadable instructions: %w", id, err)
	}

	switch jev.AnswerType(w.Type) {
	case jev.TypeNoul:
		q := jev.Noul(instructions)
		if len(w.Criteria) == 0 {
			return q, nil
		}
		var poles struct {
			True  json.RawMessage `json:"true"`
			False json.RawMessage `json:"false"`
		}
		if err := json.Unmarshal(w.Criteria, &poles); err != nil {
			return nil, fmt.Errorf("reflex: question %q: noul criteria need true and false: %w", id, err)
		}
		yes, _ := decodeAny(poles.True)
		no, _ := decodeAny(poles.False)
		if yes == nil && no == nil {
			return q, nil
		}
		return q.When(yes, no), nil

	case jev.TypeChoice:
		var options map[string]any
		if err := json.Unmarshal(w.Criteria, &options); err != nil {
			return nil, fmt.Errorf("reflex: question %q: choice criteria must be an object of options: %w", id, err)
		}
		return jev.Choice(instructions, jev.Options(options)), nil

	case jev.TypeScore:
		var levels []any
		if err := json.Unmarshal(w.Criteria, &levels); err != nil {
			return nil, fmt.Errorf("reflex: question %q: score criteria must be an ordered list of levels: %w", id, err)
		}
		return jev.Score(instructions, levels...), nil

	case "":
		return nil, fmt.Errorf("reflex: question %q has no type (want noul, choice or score)", id)
	default:
		return nil, fmt.Errorf("reflex: question %q has unknown type %q", id, w.Type)
	}
}

// decodeAny turns raw JSON into the any jev expects, keeping a bare string a
// string rather than wrapping it.
func decodeAny(raw json.RawMessage) (any, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, err
	}
	return v, nil
}
