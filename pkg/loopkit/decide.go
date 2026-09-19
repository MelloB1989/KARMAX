package loopkit

import "encoding/json"

// Asking for a judgement instead of a conversation.
//
// Kit.Ask spends a full model turn: seconds of latency and a real token bill,
// for a loop that often only wants to know whether to bother. Decide puts the
// question to a calibrated probability model instead — it answers in well under
// a second, costs a fraction, and hands back numbers rather than prose, so the
// branch lives in the loop's own code.
//
// Every question is answered in one pass against one state, and only the input
// is billed. A loop that wants five judgements should ask for five in one
// Decide rather than calling it five times.

// QuestionType names one of the three primitives.
type QuestionType string

const (
	// QuestionNoul is a yes/no question, answered with a probability.
	QuestionNoul QuestionType = "noul"
	// QuestionChoice picks one option, answered with a distribution.
	QuestionChoice QuestionType = "choice"
	// QuestionScore rates against an ordered rubric.
	QuestionScore QuestionType = "score"
)

// Question is one typed question. Build one with Noul, Choice or Score.
type Question struct {
	Type         QuestionType `json:"type"`
	Instructions any          `json:"instructions"`
	Criteria     any          `json:"criteria,omitempty"`
}

// Questions maps an id the loop chooses to the question asked under it. The
// ids come back on the answers and never reach the model.
type Questions map[string]Question

// Noul asks a yes/no question.
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
// levels are required: one cannot discriminate.
func Score(instructions string, levels ...string) Question {
	criteria := make([]any, 0, len(levels))
	for _, l := range levels {
		criteria = append(criteria, l)
	}
	return Question{Type: QuestionScore, Instructions: instructions, Criteria: criteria}
}

// Decision is one evaluation: every question answered against one state.
type Decision struct {
	// Model is the versioned id that answered. Worth logging: an alias moves
	// under you and a tuned threshold is version-specific.
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
	// Score is the probability-weighted position across the levels.
	Score float64 `json:"score"`
	// Probabilities maps every option or level to its probability.
	Probabilities map[string]float64 `json:"probabilities,omitempty"`
	// Confidence collapses the shape of Probabilities into one number. Nouls
	// do not carry one; read Noul's distance from 0.5 instead.
	Confidence float64 `json:"confidence"`
}

// Noul returns the probability for a yes/no question, or 0 if it was not asked.
func (d *Decision) Noul(id string) float64 {
	if d == nil {
		return 0
	}
	return d.Answers[id].Noul
}

// Yes reports whether a yes/no answer is above the given probability. The
// threshold is explicit because what counts as a yes depends on what a wrong
// yes costs.
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

// MarshalJSON keeps a question's wire shape stable.
func (q Question) MarshalJSON() ([]byte, error) {
	type wire Question
	return json.Marshal(wire(q))
}
