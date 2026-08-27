package api

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/MelloB1989/karmax/internal/agent"
	"github.com/MelloB1989/karmax/pkg/karmahelper"
	"go.uber.org/zap"
)

// The two places the console does more than read: acting on an approval, and
// asking a model to draft a recipe. Both go through the SAME machinery the
// rest of KARMAX uses rather than a console-only path, so an action approved
// here behaves exactly like one approved from Slack or the phone.

// consoleAgent picks the agent that will carry out a console action.
func (s *ConsoleServer) consoleAgent(id string) *agent.Agent {
	if s.agents == nil {
		return nil
	}
	if strings.TrimSpace(id) != "" {
		if a, ok := s.agents.Get(id); ok {
			return a
		}
	}
	list := s.agents.List()
	if len(list) == 0 {
		return nil
	}
	return list[0]
}

// decideProposal records an operator's decision and acts on it.
//
// Mirrors internal/api.Server.handleProposalDecision deliberately: approve
// executes, reject-with-a-note feeds the note back so the agent can rework,
// and a bare reject just drops it. The one difference is that the deciding
// operator has a name here, and it is recorded.
func (s *ConsoleServer) decideProposal(id, decision, note, by string) error {
	p, err := s.store.GetProposal(id)
	if err != nil {
		return err
	}
	if p == nil {
		return errors.New("no such approval")
	}

	if decision == "reject" {
		if err := s.store.DecideProposalBy(id, "rejected", note, by); err != nil {
			return err
		}
		if strings.TrimSpace(note) == "" {
			return nil // a bare reject drops it; there is nothing to rework
		}
		ag := s.consoleAgent(p.AgentID)
		if ag == nil {
			return nil
		}
		prompt := fmt.Sprintf(
			"The operator REJECTED your proposed action and left feedback. Rework your approach using that feedback. If a revised action still makes sense, call the `propose` tool to submit a NEW proposal that incorporates the feedback (do not repeat the rejected version). If the feedback means they don't want this at all, just acknowledge and do not re-propose.\n\nRejected title: %s\nRejected action: %s\nOperator feedback: %s",
			p.Title, p.ProposedAction, note,
		)
		go s.runDetached(ag, prompt, "", id)
		return nil
	}

	if err := s.store.DecideProposalBy(id, "approved", note, by); err != nil {
		return err
	}
	ag := s.consoleAgent(p.AgentID)
	if ag == nil {
		// The decision is recorded but nothing can carry it out; say so rather
		// than leaving it "approved" forever with no explanation.
		_ = s.store.SetProposalResult(id, "failed", "no agent was available to execute this")
		return errors.New("no agent is available to execute this")
	}

	prompt := fmt.Sprintf(
		"The operator APPROVED this proposed action. Execute it now using your tools, then confirm exactly what you did.\n\nTitle: %s\nAction: %s",
		p.Title, p.ProposedAction,
	)
	if strings.TrimSpace(note) != "" {
		prompt += "\nNote from the operator: " + note
	}
	// Executed in the background so the request returns at once; the console
	// polls until the status settles to executed or failed.
	go s.runDetached(ag, prompt, id, id)
	return nil
}

// runDetached runs one agent turn away from the request's lifetime. resultID,
// when set, receives the outcome.
func (s *ConsoleServer) runDetached(ag *agent.Agent, prompt, resultID, logID string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	reply, err := ag.Chat(ctx, prompt)
	if err != nil {
		s.log.Warn("console-approved action failed", zap.String("proposal", logID), zap.Error(err))
		if resultID != "" {
			_ = s.store.SetProposalResult(resultID, "failed", err.Error())
		}
		return
	}
	if resultID != "" {
		_ = s.store.SetProposalResult(resultID, "executed", reply)
	}
}

// generateWithModel is the model call the recipe builder uses.
//
// Runs on the configured default provider/model — the same one the agent
// thinks with — so a draft the console produces is a draft this install can
// actually run.
func (s *ConsoleServer) generateWithModel(ctx context.Context, systemPrompt, userPrompt string) (string, error) {
	if s.cfg == nil || s.cfg.AI.DefaultModel == "" {
		return "", errors.New("no default model is configured")
	}
	sess := karmahelper.NewSession(karmahelper.SessionConfig{
		Kind:         "console-recipegen",
		Provider:     s.cfg.AI.DefaultProvider,
		Model:        s.cfg.AI.DefaultModel,
		SystemPrompt: systemPrompt,
		MaxTokens:    8192,
	}, nil)

	reply, _, _, err := sess.Chat(ctx, userPrompt)
	return reply, err
}
