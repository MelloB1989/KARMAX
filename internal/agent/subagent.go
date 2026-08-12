package agent

import (
	"context"
	"fmt"

	"github.com/MelloB1989/karmax/internal/store"
	"github.com/MelloB1989/karmax/internal/tools"
	"github.com/MelloB1989/karmax/pkg/karmahelper"
	"go.uber.org/zap"
)

// Running a task on a copy of this agent.
//
// A child is a fresh model session with this agent's tools and system prompt but
// none of its conversation. That isolation is the feature: the parent's context
// stays small because the work happened somewhere else, and two children cannot
// contaminate each other's reasoning.
//
// Deliberately NOT a registered Agent. A registry entry brings a mailbox, a
// health check, a restart policy and a row in every listing — lifecycle for
// something that lives for one task. The child needs a model, tools and a
// prompt, which is what MemoryModel has done for retrieval all along.

// spawnChild runs one brief on a fresh session and returns its answer.
func (a *Agent) spawnChild(ctx context.Context, childID, brief string) (string, error) {
	a.mu.RLock()
	allTools := a.allTools
	a.mu.RUnlock()

	// The child gets every tool in full. It has no index and no tools.load,
	// because it does not get a second turn to ask for anything — one brief, one
	// answer.
	childTools := make([]tools.Tool, 0, len(allTools))
	for _, t := range allTools {
		// No spawning from a spawn. Depth is also checked in the tool, but a
		// child simply not holding the tool is the stronger guarantee.
		if t.Manifest().Name == "subagent.spawn" {
			continue
		}
		childTools = append(childTools, t)
	}

	sess := karmahelper.NewSession(karmahelper.SessionConfig{
		Provider:       a.def.Provider,
		Model:          a.def.Model,
		SystemPrompt:   a.def.SystemPrompt,
		Temperature:    a.def.Temperature,
		MaxTokens:      a.def.MaxTokens,
		FallbackModels: fallbackModelsOf(a.def),
	}, childTools)

	a.log.Info("running a sub-agent", zap.String("child", childID))
	answer, _, tokens, err := sess.Chat(ctx, brief)

	// Children spend real money and are invisible in the parent's own usage.
	if a.store != nil {
		if uerr := a.store.RecordModelUsage(usageOf(childID, a.def, "subagent", tokens)); uerr != nil {
			a.log.Warn("could not record sub-agent usage", zap.Error(uerr))
		}
	}
	if err != nil {
		return "", fmt.Errorf("sub-agent %s: %w", childID, err)
	}
	return karmahelper.CleanContent(answer), nil
}

// fallbackModelsOf converts a def's fallbacks for a session config.
func fallbackModelsOf(def AgentDef) []karmahelper.FallbackModel {
	out := make([]karmahelper.FallbackModel, 0, len(def.FallbackModels))
	for _, fb := range def.FallbackModels {
		out = append(out, karmahelper.FallbackModel{Provider: fb.Provider, Model: fb.Model})
	}
	return out
}

// usageOf builds a usage row for a sub-model call.
func usageOf(agentID string, def AgentDef, kind string, t karmahelper.TokenInfo) store.ModelUsage {
	return store.ModelUsage{
		AgentID:      agentID,
		Provider:     def.Provider,
		Model:        def.Model,
		Kind:         kind,
		InputTokens:  t.InputTokens,
		OutputTokens: t.OutputTokens,
		CacheRead:    t.CacheReadTokens,
		CacheWrite:   t.CacheWriteTokens,
	}
}
