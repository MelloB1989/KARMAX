package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/MelloB1989/karmax/internal/policy"
	"github.com/MelloB1989/karmax/internal/store"
	"github.com/MelloB1989/karmax/internal/tools"
	"github.com/google/uuid"
)

// ProposeTool creates a human-in-the-loop approval request: KARMAX proposes an
// action, it appears in the operator's phone app (with a push), and only once
// they approve does the agent execute it. This is what makes "delegate anything
// with full access" safe.
type ProposeTool struct {
	Store   *store.Store
	AgentID string
	// Policy is the operator's operating policy. Nil means the default: act,
	// then report, asking only for what is on the hard floor.
	Policy *policy.Policy
}

func (t *ProposeTool) policy() policy.Policy {
	if t.Policy != nil {
		return *t.Policy
	}
	return policy.Default()
}

func (t *ProposeTool) Manifest() tools.ToolManifest {
	return tools.ToolManifest{
		Name:        "propose",
		Description: t.description(),
		Parameters: json.RawMessage(`{
			"type": "object",
			"properties": {
				"title": {"type": "string", "description": "Short action title, e.g. 'Follow up with Acme on the invoice'."},
				"summary": {"type": "string", "description": "Why this matters and the context that prompted it."},
				"action": {"type": "string", "description": "The concrete action you will take on approval, including any draft text (e.g. the exact WhatsApp message)."},
				"kind": {"type": "string", "description": "Optional category: message, schedule, purchase, task, other."},
				"urgency": {"type": "string", "enum": ["low", "normal", "high"], "description": "How time-sensitive (default normal)."},
				"floor": {"type": "string", "description": "Which hard-floor rule this falls under, in the policy's words (e.g. 'payments or spending money'). Required under act-then-report: a proposal that names none is declined, because the operator does not want to be asked about anything else."}
			},
			"required": ["title", "action"]
		}`),
	}
}

func (t *ProposeTool) Execute(ctx context.Context, input map[string]any) (tools.ToolResult, error) {
	title, _ := input["title"].(string)
	action, _ := input["action"].(string)
	if strings.TrimSpace(title) == "" || strings.TrimSpace(action) == "" {
		return tools.ErrorResult(fmt.Errorf("title and action are required")), nil
	}
	summary, _ := input["summary"].(string)
	kind, _ := input["kind"].(string)
	urgency, _ := input["urgency"].(string)
	floor, _ := input["floor"].(string)
	floor = strings.TrimSpace(floor)

	// Under act-then-report the operator is asked about the hard floor and
	// nothing else. A proposal for anything else is a question they decided
	// not to be asked: decline it, and say so in a way that gets the work done.
	pol := t.policy()
	if pol.ActFirst() && floor == "" {
		return tools.SuccessResult(map[string]any{
			"status": "not_needed",
			"message": "Not proposed: under the operator's policy (act, then report) this needs no approval. Do it now; " +
				"what you send as the operator is reported to them. Only the hard floor is asked about: " +
				strings.Join(pol.HardFloor(), "; ") + ". If this really is one of those, call propose again with floor set to the rule.",
		}), nil
	}

	id, err := CreateProposalOnFloor(t.Store, t.AgentID, kind, title, summary, action, urgency, floor)
	if err != nil {
		return tools.ErrorResult(fmt.Errorf("create proposal: %w", err)), nil
	}

	return tools.SuccessResult(map[string]any{
		"status":      "pending_approval",
		"proposal_id": id,
		"message":     "Proposed to the operator for approval. Wait for their decision before acting.",
	}), nil
}

// CreateProposal writes a pending approval to the store and hands it to the
// observer of approvals, which applies the operating policy: under "act, then
// report" one that is not on the hard floor is carried out, and one that waits
// for the operator is pushed to their phone (see MirrorEvent.Push). It is the
// single entry point
// for EVERYTHING that needs the operator's decision — the propose tool, the
// proactive WhatsApp proxy, and loops — so decisions always land in the
// approvals inbox (actionable: approve → execute), never as plain
// notifications.
func CreateProposal(s *store.Store, agentID, kind, title, summary, action, urgency string) (string, error) {
	return CreateProposalOnFloor(s, agentID, kind, title, summary, action, urgency, "")
}

// CreateProposalOnFloor is CreateProposal for an approval its raiser knows is
// on the hard floor, naming the rule. Empty floor means unknown.
func CreateProposalOnFloor(s *store.Store, agentID, kind, title, summary, action, urgency, floor string) (string, error) {
	// Drop empty decisions. The scan loops (chat-sweep/wa-monitor/gchat-watch)
	// sometimes get a placeholder APPROVE line from the harness ("APPROVE: none",
	// "APPROVE (none)", "APPROVE — none") that really means "nothing to approve".
	// Those produced blank "Decision — (none)" approvals. If neither the title
	// (minus the "Decision — " prefix) nor the action carries real content,
	// there's nothing to decide — skip it.
	titleTail := title
	for _, p := range []string{"Decision — ", "Decision —", "Decision - ", "Decision – ", "Decision: "} {
		if strings.HasPrefix(titleTail, p) {
			titleTail = titleTail[len(p):]
			break
		}
	}
	if meaningfulText(titleTail) == "" && meaningfulText(action) == "" {
		return "", nil
	}

	// Dedup: if the same decision is already pending (or was raised in the last
	// 12h), don't create another. The proxy/scan loops re-flag the same items on
	// every run; without this the approvals inbox floods with duplicates.
	if dup, _ := s.HasSimilarProposal(title, 12*time.Hour); dup {
		return "", nil
	}
	id := uuid.New().String()
	if err := s.CreateProposal(store.StoredProposal{
		ID:             id,
		AgentID:        agentID,
		Kind:           kind,
		Title:          title,
		Summary:        summary,
		ProposedAction: action,
		Status:         "pending",
	}); err != nil {
		return "", err
	}

	priority := "high"
	if urgency == "low" {
		priority = "default"
	}
	push := func() {
		_, _, _ = SendExpoPush(s, "Approval needed", title, priority, map[string]any{
			"type":        "proposal",
			"proposal_id": id,
		})
	}
	body := action
	if strings.TrimSpace(summary) != "" {
		body = summary + "\n\n" + action
	}
	// The observer applies the operating policy — a proposal it clears needs
	// no one and is not announced — so the phone push is its to send. With no
	// observer the approval waits, and the phone is told now.
	if !notifyMirror(MirrorEvent{Kind: MirrorApproval, Title: title, Body: body, Source: "proposal:" + kind,
		ProposalID: id, Floor: floor, Push: push}) {
		push()
	}
	return id, nil
}

// meaningfulText returns the input with leading/trailing separator noise and
// wrapping punctuation stripped, or "" if what remains is empty or a
// placeholder ("none", "n/a", "nothing needed", …). Used to detect empty
// approvals that carry no real decision.
func meaningfulText(s string) string {
	s = strings.TrimSpace(s)
	s = strings.Trim(s, " \t:—–-()[]{}\"'.")
	s = strings.TrimSpace(s)
	switch strings.ToLower(s) {
	case "", "none", "n/a", "na", "nil", "null", "nothing",
		"nothing needed", "no action", "no action needed",
		"none needed", "nothing to do", "no reply needed", "skip":
		return ""
	}
	return s
}

// description is the tool's contract, from the policy it enforces.
func (t *ProposeTool) description() string {
	pol := t.policy()
	floor := strings.Join(pol.HardFloor(), "; ")
	if pol.ActFirst() {
		return "Ask the operator to approve an action BEFORE doing it — ONLY for the hard floor: " + floor +
			". Set floor to the rule it falls under. It reaches them on WhatsApp and lands in their app's approvals; do not act until they approve, " +
			"and you will be asked to execute it. Everything else — messages to other people as the operator, replies, comments, " +
			"scheduling, changes that can be undone — needs no approval under their policy (act, then report): just do it. " +
			"A proposal without a floor rule is declined."
	}
	return "Ask the operator to approve a consequential action BEFORE doing it, with a concrete draft (the exact message, " +
		"the command). It lands in their app's approvals and reaches them on WhatsApp; do not act until they approve. " +
		"Always required for: " + floor + " (set floor to the rule). Not needed for reading, searching, drafting or filing memory."
}
