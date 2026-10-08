package reflex

import (
	"fmt"
	"time"

	"github.com/MelloB1989/karma/ai/jev"
)

// Action is what the orchestrator does with an event.
type Action string

const (
	// ActionDrop discards the event without waking the agent.
	ActionDrop Action = "drop"
	// ActionRemember files the event in long-term memory and stops there.
	ActionRemember Action = "remember"
	// ActionHandle runs a normal agent turn.
	ActionHandle Action = "handle"
	// ActionDelegate routes straight to a coding harness.
	ActionDelegate Action = "delegate"
	// ActionEscalate interrupts the operator.
	ActionEscalate Action = "escalate"
)

// Effort is how much machinery the event deserves.
type Effort string

const (
	EffortTrivial Effort = "trivial"
	EffortNormal  Effort = "normal"
	EffortHeavy   Effort = "heavy"
)

// Thresholds turn probabilities into decisions. They are version-specific:
// retune them when the jev model alias moves under you.
type Thresholds struct {
	// Drop is the probability mass a silencing verdict — drop or remember —
	// must hold before it is acted on.
	Drop float64
	// Remember is the noul above which a fact is worth storing.
	Remember float64
	// Risk is the normalised risk above which acting needs approval.
	Risk float64
	// Escalate is the normalised urgency above which the operator is interrupted.
	Escalate float64
}

// DefaultThresholds is a starting point, not a recommendation.
var DefaultThresholds = Thresholds{Drop: 0.75, Remember: 0.60, Risk: 0.66, Escalate: 0.90}

func (t Thresholds) withDefaults() Thresholds {
	d := DefaultThresholds
	if t.Drop > 0 {
		d.Drop = t.Drop
	}
	if t.Remember > 0 {
		d.Remember = t.Remember
	}
	if t.Risk > 0 {
		d.Risk = t.Risk
	}
	if t.Escalate > 0 {
		d.Escalate = t.Escalate
	}
	return d
}

// Hint is what the caller knows about an event that the event itself does not
// carry. Operator is the one field with teeth: it is established from who sent
// the message, never from anything the model said.
type Hint struct {
	Operator bool
	Channel  string
	Sender   string
	Recent   []string
}

// Verdict is one screening decision, plus the numbers behind it so a threshold
// can be retuned against what actually happened.
type Verdict struct {
	Action          Action  `json:"action"`
	Effort          Effort  `json:"effort"`
	Urgency         float64 `json:"urgency"`
	Risk            float64 `json:"risk"`
	Remember        bool    `json:"remember"`
	RequireApproval bool    `json:"require_approval"`
	Confidence      float64 `json:"confidence"`
	// Mass is the probability on the chosen disposition. This is what a
	// threshold is cut against, not Confidence.
	Mass        float64       `json:"mass"`
	Reason      string        `json:"reason"`
	Model       string        `json:"model,omitempty"`
	Elapsed     time.Duration `json:"elapsed"`
	InputTokens int           `json:"input_tokens,omitempty"`
	// FailedOpen marks a verdict that was not screened at all — no key, no
	// answer, or an error. The event flows exactly as it did before reflex.
	FailedOpen bool `json:"failed_open"`
	// Floored marks a verdict the operator floor promoted.
	Floored bool `json:"floored,omitempty"`
	// Raw is every answer, for calibration.
	Raw map[string]float64 `json:"raw,omitempty"`
}

// Silent reports whether the verdict means nothing reaches the agent.
func (v Verdict) Silent() bool { return v.Action == ActionDrop || v.Action == ActionRemember }

// openVerdict is what every failure path returns: handle it the old way.
func openVerdict(reason string) Verdict {
	return Verdict{Action: ActionHandle, Effort: EffortNormal, Reason: reason, FailedOpen: true}
}

// Decide turns one evaluation into a verdict. Pure: no network, no clock, no
// logging, so the policy can be tested against a hand-built Result.
func Decide(res *jev.Result, hint Hint, th Thresholds) Verdict {
	if res == nil {
		return openVerdict("no evaluation")
	}
	th = th.withDefaults()

	disposition, confidence, err := res.Choice(QDisposition)
	if err != nil {
		return openVerdict("no disposition: " + err.Error())
	}

	v := Verdict{
		Action:     Action(disposition),
		Effort:     EffortNormal,
		Confidence: confidence,
		Model:      res.Model,
		Raw:        rawAnswers(res),
	}
	if !validAction(v.Action) {
		return openVerdict(fmt.Sprintf("unknown disposition %q", disposition))
	}
	// How much mass is actually on the chosen option, which is a different
	// question from how concentrated the distribution is.
	//
	// Confidence was used for this at first and it was the wrong number. Across
	// five options a clear winner still scores around 0.5 on shape, so a bar set
	// where a silencing verdict ought to sit was one almost nothing cleared: in
	// the first live traffic, drop fired once in thirty-three. Meanwhile a
	// remember silenced an event at 0.24. The option's own probability says what
	// the threshold is meant to ask — how sure is the model that THIS is right.
	v.Mass = optionMass(res, QDisposition, disposition)

	if urgency, _, err := res.Score(QUrgency); err == nil {
		v.Urgency = normalise(urgency, urgencyLevels)
	}
	if risk, _, err := res.Score(QRisk); err == nil {
		v.Risk = normalise(risk, riskLevels)
	}
	if remember, err := res.Noul(QRemember); err == nil {
		v.Remember = remember >= th.Remember
	}
	if effort, _, err := res.Choice(QEffort); err == nil && validEffort(Effort(effort)) {
		v.Effort = Effort(effort)
	}

	// Something worth keeping is never discarded, only demoted to filing it.
	if v.Action == ActionDrop && v.Remember {
		v.Action = ActionRemember
		v.Reason = "carries a fact worth keeping"
	}

	// A silencing verdict the model is not sure about is not a verdict.
	//
	// Drop and remember are the two that end an event without a brain ever
	// seeing it, so both must clear the bar — remember included, because filing
	// something and answering nobody is just as silent as discarding it, and it
	// additionally puts a stranger's chatter in the operator's memory.
	if v.Silent() && v.Mass < th.Drop {
		v.Reason = fmt.Sprintf("%s held %.2f of the mass, below %.2f", v.Action, v.Mass, th.Drop)
		v.Action = ActionHandle
	}

	v.RequireApproval = v.Risk >= th.Risk
	if v.Action == ActionHandle && v.Urgency >= th.Escalate {
		v.Action = ActionEscalate
		v.Reason = fmt.Sprintf("urgency %.2f at or above %.2f", v.Urgency, th.Escalate)
	}

	// The operator floor. Nothing the operator says is ever dropped or merely
	// filed: reflex chooses HOW their message is handled, never whether.
	if hint.Operator && v.Silent() {
		v.Action = ActionHandle
		v.Floored = true
		v.Reason = "operator floor: " + string(Action(disposition))
	}

	if v.Reason == "" {
		v.Reason = fmt.Sprintf("%s at %.2f mass", v.Action, v.Mass)
	}
	return v
}

// optionMass returns the probability the model put on one option.
func optionMass(res *jev.Result, id, option string) float64 {
	probs, err := res.Probabilities(id)
	if err != nil {
		return 0
	}
	return probs[option]
}

func validAction(a Action) bool {
	switch a {
	case ActionDrop, ActionRemember, ActionHandle, ActionDelegate, ActionEscalate:
		return true
	}
	return false
}

func validEffort(e Effort) bool {
	switch e {
	case EffortTrivial, EffortNormal, EffortHeavy:
		return true
	}
	return false
}

// normalise maps a weighted score across n ordered levels onto 0..1.
func normalise(score float64, levels int) float64 {
	if levels < 2 {
		return 0
	}
	v := score / float64(levels-1)
	switch {
	case v < 0:
		return 0
	case v > 1:
		return 1
	}
	return v
}

// rawAnswers flattens a result into the numbers worth keeping for calibration.
func rawAnswers(res *jev.Result) map[string]float64 {
	out := make(map[string]float64, len(res.Answers)*2)
	for id, a := range res.Answers {
		switch a.Type {
		case jev.TypeNoul:
			out[id] = a.Noul
		case jev.TypeScore:
			out[id] = a.Score
			out[id+".confidence"] = a.Confidence
		case jev.TypeChoice:
			out[id+".confidence"] = a.Confidence
			for opt, p := range a.Probabilities {
				out[id+"."+opt] = p
			}
		}
	}
	return out
}
