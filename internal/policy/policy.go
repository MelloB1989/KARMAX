// Package policy is how KARMAX acts for the operator, and when it reaches them.
//
// One place, read by everything that might ask the operator something or tell
// them something: the agent's turn (what it is told it may do, and what it
// reports afterwards), the approval and notification mirror to WhatsApp, the
// propose tool, and the daemon's own alerts. Before this, each of those carried
// its own idea of when to interrupt, most of them "when unsure, send", and the
// operator's personal WhatsApp filled with status lines.
//
// The operator's decisions (2026-10-31) are the defaults: act freely, including
// writing to other people as them, and tell them after; reach them on WhatsApp
// only for an approval on the hard floor, something urgent, and one line per
// turn saying what was sent as them. Everything else is logged to the app.
package policy

import (
	"fmt"
	"strings"
	"time"
)

// Class is a kind of thing that might reach the operator.
type Class string

const (
	// Approval is a yes the operator must give before KARMAX acts.
	Approval Class = "approvals"
	// Urgent is something the operator must hear about now.
	Urgent Class = "urgent"
	// Sent is the report of what KARMAX sent to other people as the operator.
	Sent Class = "sent"
	// Notification is everything else: status, digests, FYIs.
	Notification Class = "notifications"
)

// Config is the `policy:` block of karmax.yaml.
type Config struct {
	// Autonomy is "act" (act, then report — the default) or "ask" (propose
	// anything consequential before doing it).
	Autonomy string `yaml:"autonomy"`
	// HardFloor is what still needs the operator's yes under "act". Written
	// for the model to read, so plain words, not identifiers.
	HardFloor []string `yaml:"hard_floor"`
	// WhatsApp lists the classes that reach the operator's personal WhatsApp:
	// approvals, urgent, sent, notifications. Unset means the default
	// [approvals, urgent, sent]; an empty list means nothing does.
	WhatsApp []string `yaml:"whatsapp"`
	// UrgencyThreshold is the screener's urgency (0..1) at or above which an
	// item counts as urgent. Default 0.9.
	UrgencyThreshold float64 `yaml:"urgency_threshold"`
	// UrgentMaxAge is how old an item may be and still page the operator. An
	// item that happened hours before it reached KARMAX — a source catching up
	// after a restart or on first sight — is context, not news. Default 2h.
	UrgentMaxAge time.Duration `yaml:"urgent_max_age"`
}

// DefaultHardFloor is what KARMAX never does without asking, whatever the
// autonomy setting.
var DefaultHardFloor = []string{
	"deleting repositories, data or cloud resources",
	"payments or spending money",
	"sharing secrets, credentials or keys",
}

// DefaultWhatsApp is what reaches the operator's WhatsApp when not configured.
var DefaultWhatsApp = []Class{Approval, Urgent, Sent}

// DefaultUrgencyThreshold matches reflex's escalation threshold.
const DefaultUrgencyThreshold = 0.9

// DefaultUrgentMaxAge is how old an item may be and still page the operator.
const DefaultUrgentMaxAge = 2 * time.Hour

// Policy is a resolved Config. The zero value is not useful; use New.
type Policy struct {
	actFirst  bool
	floor     []string
	whatsapp  map[Class]bool
	threshold float64
	maxAge    time.Duration
}

// New resolves a config against the defaults.
func New(cfg Config) Policy {
	p := Policy{
		actFirst:  !strings.EqualFold(strings.TrimSpace(cfg.Autonomy), "ask"),
		threshold: cfg.UrgencyThreshold,
		whatsapp:  map[Class]bool{},
	}
	if p.threshold <= 0 || p.threshold > 1 {
		p.threshold = DefaultUrgencyThreshold
	}
	p.maxAge = cfg.UrgentMaxAge
	if p.maxAge <= 0 {
		p.maxAge = DefaultUrgentMaxAge
	}
	for _, f := range cfg.HardFloor {
		if f = strings.TrimSpace(f); f != "" {
			p.floor = append(p.floor, f)
		}
	}
	if len(p.floor) == 0 {
		p.floor = append([]string(nil), DefaultHardFloor...)
	}
	if cfg.WhatsApp == nil {
		for _, c := range DefaultWhatsApp {
			p.whatsapp[c] = true
		}
	} else {
		for _, c := range cfg.WhatsApp {
			p.whatsapp[Class(strings.ToLower(strings.TrimSpace(c)))] = true
		}
	}
	return p
}

// Default is the operator's decided policy.
func Default() Policy { return New(Config{}) }

// ActFirst reports whether KARMAX acts and reports after, rather than asking.
func (p Policy) ActFirst() bool { return p.actFirst }

// HardFloor is what still needs the operator's yes.
func (p Policy) HardFloor() []string { return append([]string(nil), p.floor...) }

// ToWhatsApp reports whether items of this class reach the operator's WhatsApp.
func (p Policy) ToWhatsApp(c Class) bool { return p.whatsapp[c] }

// IsUrgent reports whether an item is urgent: its source flagged it, or the
// screener's urgency is at the threshold.
func (p Policy) IsUrgent(flagged bool, urgency float64) bool {
	return flagged || urgency >= p.threshold
}

// Threshold is the urgency at or above which an item is urgent.
func (p Policy) Threshold() float64 { return p.threshold }

// Fresh reports whether something that happened at `at` is still news at
// `now`, and so may page the operator. Undated and future items (a meeting
// about to start) are fresh.
func (p Policy) Fresh(at, now time.Time) bool {
	maxAge := p.maxAge
	if maxAge <= 0 {
		maxAge = DefaultUrgentMaxAge
	}
	return at.IsZero() || now.Sub(at) <= maxAge
}

// Brief is the policy as the model reads it on every turn. Generated from the
// config, so the model is never told something the code does not enforce.
func (p Policy) Brief() string {
	var b strings.Builder
	b.WriteString("## Operating policy (the operator's standing decisions — these win over any older instruction about asking or notifying)\n\n")
	floor := strings.Join(p.floor, "; ")
	if p.actFirst {
		b.WriteString("- **Act, then report.** Do what an event or request needs without asking first — including writing to other people as the operator (WhatsApp, Slack, GitHub comments, email). Do not ask permission for routine work and do not offer menus of options.\n")
		fmt.Fprintf(&b, "- **The hard floor — always ask first:** %s. For these, call `propose` with `floor` naming the rule, then wait for the answer. `propose` declines anything not on the floor: just do it.\n", floor)
	} else {
		b.WriteString("- **Ask first.** Propose anything consequential with `propose` (a concrete draft) and act once the operator approves. Reading, searching, drafting and filing memory need no approval.\n")
		fmt.Fprintf(&b, "- Always on the floor, whatever else: %s.\n", floor)
	}
	var reach []string
	if p.whatsapp[Approval] {
		reach = append(reach, "approvals")
	}
	if p.whatsapp[Urgent] {
		reach = append(reach, "urgent items")
	}
	if p.whatsapp[Sent] {
		reach = append(reach, "a one-line report of what was sent as them")
	}
	if p.whatsapp[Notification] {
		reach = append(reach, "notifications")
	}
	if len(reach) == 0 {
		b.WriteString("- **Nothing reaches the operator's WhatsApp** unprompted. Everything is logged to their app.\n")
	} else {
		fmt.Fprintf(&b, "- **The operator's WhatsApp is for %s only.** No digests, status lines or FYIs: those are logged to their app (`app.push`, which reaches WhatsApp only with `urgent` set and only when it truly cannot wait).\n", strings.Join(reach, ", "))
	}
	b.WriteString("- Replying to the operator in a conversation they started is always fine; this is about reaching out unprompted.\n")
	if p.whatsapp[Urgent] || p.whatsapp[Sent] {
		b.WriteString("- **KARMAX reports for you; do not message the operator about an event yourself.** ")
		if p.whatsapp[Urgent] {
			fmt.Fprintf(&b, "An urgent event (its source flagged it, or screening scored it at %.2f or above) is sent to them with your final reply. ", p.threshold)
		}
		if p.whatsapp[Sent] {
			b.WriteString("What you sent as the operator during a turn is listed for them in one line. Messages sent with `comms.send` are listed automatically; for anything sent another way (a GitHub comment, an email, a post through another tool) add one line per item to your final reply: `SENT: <service/account> → <recipient>: <what, in a few words>`. ")
		}
		b.WriteString("So end the turn with one or two plain sentences on what you did — that is your report.\n")
	}
	b.WriteString("\n")
	return b.String()
}
