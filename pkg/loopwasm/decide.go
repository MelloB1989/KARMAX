package loopwasm

import (
	"encoding/json"
	"errors"
)

// Asking for a judgement instead of a conversation.
//
// Ask spends a full agent turn — seconds, and a real token bill — on questions
// that are often just "is this worth bothering with". Decide puts the question
// to a calibrated probability model instead: it answers in well under a second
// and hands back numbers, so the branch stays in the loop's own code rather
// than in a sentence somebody has to parse.
//
// Every question is answered in one pass against one state, and only the input
// is billed. Asking five questions in one call costs barely more than one, and
// far less than five calls — so ask them together.
//
//	d, err := loopwasm.Decide(msg, loopwasm.Questions{
//	    "reply": loopwasm.Noul("Does this need a reply from the assistant?").
//	        When("It asks something or expects action", "Small talk or an acknowledgement"),
//	    "lane": loopwasm.Choice("Who should handle it?", map[string]string{
//	        "assistant": "it can be answered from what is known",
//	        "operator":  "only the operator can decide",
//	    }),
//	})
//	if err != nil || !d.Yes("reply", 0.7) {
//	    return
//	}

const fnDecide = "decide"

// ErrNoQuestions means Decide was called with nothing to answer.
var ErrNoQuestions = errors.New("loopwasm: decide needs at least one question")

// QuestionType names one of the three primitives.
type QuestionType string

const (
	QuestionNoul   QuestionType = "noul"
	QuestionChoice QuestionType = "choice"
	QuestionScore  QuestionType = "score"
)

// Question is one typed question. Build one with Noul, Choice or Score.
type Question struct {
	Type         QuestionType `json:"type"`
	Instructions any          `json:"instructions"`
	Criteria     any          `json:"criteria,omitempty"`
}

// Questions maps an id you choose to the question asked under it. The ids come
// back on the answers and are never sent to the model, so name them for the
// code that reads them.
type Questions map[string]Question

// Noul asks a yes/no question, answered with a probability rather than a bare
// boolean — so a loop can act on how sure the model is.
func Noul(instructions string) Question {
	return Question{Type: QuestionNoul, Instructions: instructions}
}

// When describes what a yes and a no mean, which sharpens the boundary on
// questions where it is not obvious.
func (q Question) When(yes, no string) Question {
	q.Criteria = map[string]any{"true": yes, "false": no}
	return q
}

// Choice picks one of options, each mapped to when it applies. An empty
// description means the option name speaks for itself.
func Choice(instructions string, options map[string]string) Question {
	criteria := make(map[string]any, len(options))
	for name, when := range options {
		if when == "" {
			criteria[name] = nil
			continue
		}
		criteria[name] = when
	}
	return Question{Type: QuestionChoice, Instructions: instructions, Criteria: criteria}
}

// Score rates against an ordered rubric, lowest level first. At least two
// levels are required: one level cannot discriminate.
func Score(instructions string, levels ...string) Question {
	criteria := make([]any, 0, len(levels))
	for _, l := range levels {
		criteria = append(criteria, l)
	}
	return Question{Type: QuestionScore, Instructions: instructions, Criteria: criteria}
}

// Decision is one evaluation: every question answered against one state.
type Decision struct {
	// Model is the versioned id that answered. A model alias moves under you
	// and a tuned threshold is version-specific, so this is worth logging.
	Model   string            `json:"model"`
	Answers map[string]Answer `json:"answers"`
}

// Answer is one question's answer. Which fields carry meaning depends on Type.
type Answer struct {
	Type QuestionType `json:"type"`
	// Noul is the yes/no answer, 0 (no) to 1 (yes).
	Noul float64 `json:"noul"`
	// Choice is the highest-probability option.
	Choice string `json:"choice,omitempty"`
	// Score is the probability-weighted position across the levels, which can
	// land between two of them.
	Score float64 `json:"score"`
	// Probabilities maps every option or level to its probability.
	Probabilities map[string]float64 `json:"probabilities,omitempty"`
	// Confidence collapses the shape of Probabilities into one number, 0 to 1.
	// A flat distribution means the model is telling you it does not know,
	// which is a signal worth acting on. Nouls do not carry one — read Noul's
	// distance from 0.5 instead.
	Confidence float64 `json:"confidence"`
}

// Decide answers questions about state. State is the material being judged: a
// string, or anything that marshals to a JSON object when the judgement needs
// named fields.
//
// An error means the model was unavailable, not that the answer was no. A loop
// should treat it as "do whatever you did before this call existed".
func Decide(state any, questions Questions) (*Decision, error) {
	if len(questions) == 0 {
		return nil, ErrNoQuestions
	}
	req, err := json.Marshal(map[string]any{"state": state, "questions": questions})
	if err != nil {
		return nil, err
	}
	out, err := request(fnDecide, string(req))
	if err != nil {
		return nil, err
	}
	var d Decision
	if err := json.Unmarshal(out, &d); err != nil {
		return nil, err
	}
	return &d, nil
}

// Noul returns the probability for a yes/no question. A question that was not
// asked, or not answered, reads as 0.
func (d *Decision) Noul(id string) float64 {
	if d == nil {
		return 0
	}
	return d.Answers[id].Noul
}

// Yes reports whether a yes/no answer is at or above a probability. The bar is
// explicit because what counts as a yes depends on what a wrong yes costs.
func (d *Decision) Yes(id string, above float64) bool {
	return d.Noul(id) >= above
}

// Choice returns the winning option and its confidence.
func (d *Decision) Choice(id string) (string, float64) {
	if d == nil {
		return "", 0
	}
	a := d.Answers[id]
	return a.Choice, a.Confidence
}

// Score returns the weighted level and its confidence.
func (d *Decision) Score(id string) (float64, float64) {
	if d == nil {
		return 0, 0
	}
	a := d.Answers[id]
	return a.Score, a.Confidence
}
