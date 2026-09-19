package reflex

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/MelloB1989/karma/ai/jev"
	"github.com/MelloB1989/karmax/internal/bus"
)

// Question ids. They are ours and never reach the model, so they are named for
// the code that reads them.
const (
	QDisposition = "disposition"
	QUrgency     = "urgency"
	QRisk        = "risk"
	QRemember    = "remember"
	QEffort      = "effort"
)

// Level counts, kept beside the rubrics so normalise cannot drift from them.
const (
	urgencyLevels = 4
	riskLevels    = 4
)

// Sheet is the whole decision asked in one pass. Only input tokens are billed,
// so a question that is sometimes useful costs almost nothing to always ask —
// which is why this is one call and not five.
func Sheet() jev.Questions {
	return jev.Questions{
		QDisposition: jev.Choice("An autonomous assistant received this event. What should it do with it?", jev.Options{
			string(ActionDrop):     "Nothing at all. Noise, a duplicate, an automated notice, a bare acknowledgement, or something already handled.",
			string(ActionRemember): "Nothing now, but it states a durable fact about the operator, their work or their contacts that is worth filing.",
			string(ActionHandle):   "Think about it and respond or act, using ordinary judgement and tools.",
			string(ActionDelegate): "Hand to a coding harness: it needs the shell, files, code, or real research.",
			string(ActionEscalate): "Interrupt the operator directly. Time-critical, or it needs a decision only they can make.",
		}),

		QUrgency: jev.Score("How soon must this be dealt with?",
			"Never; it keeps indefinitely",
			"Whenever convenient, within days",
			"Today",
			"Immediately; a delay causes harm"),

		QRisk: jev.Score("How consequential is it if the assistant acts on this and gets it wrong?",
			"Harmless; trivially undone",
			"Mildly embarrassing but reversible",
			"Hard to undo: money, a commitment, or a message to a third party",
			"Irreversible or damaging: data loss, a public statement, a broken relationship"),

		QRemember: jev.Noul("Does this state a durable fact worth keeping in long-term memory?").
			When("A lasting fact about a person, project, preference, deadline or decision",
				"Transient chatter, a pleasantry, or something true only right now"),

		QEffort: jev.Choice("How much machinery does answering this actually need?", jev.Options{
			string(EffortTrivial): "A one-line answer from what is already here. No tools, no research.",
			string(EffortNormal):  "Ordinary reasoning, memory and a tool or two.",
			string(EffortHeavy):   "Multi-step work: shell, files, code, or sustained research.",
		}),
	}
}

// maxFieldRunes bounds one field of the state. The whole request is capped at
// 64k tokens, and a pasted log would otherwise spend the budget on itself.
const maxFieldRunes = 4000

// StateOf renders an event as the material jev judges. A JSON object rather
// than prose: the field names are part of the question.
func StateOf(evt bus.Event, hint Hint) map[string]any {
	state := map[string]any{
		"kind": string(evt.Kind),
		"age":  age(evt.Timestamp),
	}
	if hint.Operator {
		state["from"] = "the operator themselves"
	} else if hint.Sender != "" {
		state["from"] = clip(hint.Sender)
	}
	if hint.Channel != "" {
		state["channel"] = clip(hint.Channel)
	}
	if len(hint.Recent) > 0 {
		recent := hint.Recent
		if len(recent) > 8 {
			recent = recent[len(recent)-8:]
		}
		trimmed := make([]string, 0, len(recent))
		for _, r := range recent {
			trimmed = append(trimmed, clip(r))
		}
		state["recent_context"] = trimmed
	}
	if payload := summarisePayload(evt.Payload); len(payload) > 0 {
		state["payload"] = payload
	}
	return state
}

// summarisePayload keeps the payload readable and bounded. Values that are not
// scalars are rendered as compact JSON rather than dropped, because the shape
// of a payload is often the thing that identifies it as noise.
func summarisePayload(in map[string]any) map[string]any {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]any, len(in))
	for k, v := range in {
		switch k {
		case "karmax_channel_id", "agent_id":
			continue
		}
		switch t := v.(type) {
		case nil:
			continue
		case string:
			if t == "" {
				continue
			}
			out[k] = clip(t)
		case bool, float64, int, int64:
			out[k] = t
		default:
			encoded, err := json.Marshal(t)
			if err != nil {
				continue
			}
			out[k] = clip(string(encoded))
		}
	}
	return out
}

func clip(s string) string {
	s = strings.TrimSpace(s)
	if runes := []rune(s); len(runes) > maxFieldRunes {
		return string(runes[:maxFieldRunes]) + "…"
	}
	return s
}

// age describes how old an event is in words, because "2m ago" is a fact the
// model can weigh and a timestamp is not.
func age(ts time.Time) string {
	if ts.IsZero() {
		return "unknown"
	}
	d := time.Since(ts)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return d.Round(time.Minute).String() + " ago"
	case d < 24*time.Hour:
		return d.Round(time.Hour).String() + " ago"
	default:
		return d.Round(time.Hour).String() + " ago (stale)"
	}
}
