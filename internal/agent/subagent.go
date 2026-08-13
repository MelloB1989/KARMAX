package agent

import (
	"context"
	"fmt"
	"strings"

	"github.com/MelloB1989/karmax/internal/store"
	"github.com/MelloB1989/karmax/internal/tools"
	"github.com/MelloB1989/karmax/internal/tools/builtin"
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

	childTools := make([]tools.Tool, 0, len(allTools))
	for _, t := range allTools {
		// No spawning from a spawn. Depth is also checked in the tool, but a
		// child simply not holding the tool is the stronger guarantee.
		if t.Manifest().Name == "subagent.spawn" {
			continue
		}
		childTools = append(childTools, t)
	}

	// Same split the parent runs on: a core set in full, the rest named. Handing
	// a child all seventy-odd schemas would cost more per run than the work and
	// makes it likelier to reach for the wrong one.
	held, indexed := a.splitToolSet(childTools)
	if len(indexed) > 0 {
		held = append(held, &builtin.LoadToolTool{Available: manifestNames(indexed)})
	}

	sess := karmahelper.NewSession(karmahelper.SessionConfig{
		Provider:       a.def.Provider,
		Model:          a.def.Model,
		SystemPrompt:   a.def.SystemPrompt + childToolNote(held, indexed),
		Temperature:    a.def.Temperature,
		MaxTokens:      a.def.MaxTokens,
		FallbackModels: fallbackModelsOf(a.def),
	}, held)

	a.log.Info("running a sub-agent",
		zap.String("child", childID),
		zap.Int("held", len(held)), zap.Int("indexed", len(indexed)))

	answer, calls, tokens, err := sess.Chat(ctx, brief)
	if err == nil {
		answer, tokens = a.finishChildLoads(ctx, sess, answer, calls, tokens)
	}

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

// finishChildLoads continues a child that stopped to ask for a tool.
//
// The parent does this through its own session; a child has no event loop to
// re-enter, so the same two rounds happen inline. Without it the index is a
// list of things the child is told about and cannot ever call, which is worse
// than not listing them.
func (a *Agent) finishChildLoads(ctx context.Context, sess *karmahelper.Session, answer string,
	calls []karmahelper.ToolCallRecord, tokens karmahelper.TokenInfo) (string, karmahelper.TokenInfo) {

	for round := 0; round < maxToolLoadRounds; round++ {
		want := requestedTools(calls)
		if len(want) == 0 {
			return answer, tokens
		}
		lent := a.lendNamed(want)
		if len(lent) == 0 {
			a.log.Warn("a sub-agent asked for tools that cannot be bound", zap.Strings("tools", want))
			return answer, tokens
		}
		resp, tc, t2, err := sess.ChatWithExtraTools(ctx,
			"The tools you requested are now available. Use them to finish the task.", lent)
		// Tokens accumulate across rounds: each round is a billed call, and
		// reporting only the last one would undercount every child that asked.
		tokens = addTokens(tokens, t2)
		if err != nil || strings.TrimSpace(resp) == "" {
			return answer, tokens
		}
		answer, calls = resp, tc
	}
	return answer, tokens
}

func addTokens(a, b karmahelper.TokenInfo) karmahelper.TokenInfo {
	return karmahelper.TokenInfo{
		InputTokens:      a.InputTokens + b.InputTokens,
		OutputTokens:     a.OutputTokens + b.OutputTokens,
		CacheReadTokens:  a.CacheReadTokens + b.CacheReadTokens,
		CacheWriteTokens: a.CacheWriteTokens + b.CacheWriteTokens,
	}
}

// childToolNote tells the child exactly what it is holding.
//
// It inherits a system prompt written for the parent, which names tools in prose
// and assumes an index it can ask to extend. The child has neither: one turn, a
// fixed set, no tools.load. Left to infer, it invents plausible names — the
// failure looked like a broken tool bridge and was a child guessing.
func childToolNote(held, indexed []tools.Tool) string {
	var b strings.Builder
	b.WriteString("\n\n## Your tools, as a sub-agent\n\n")
	b.WriteString("You are a sub-agent: one brief, one answer, and nobody to ask a follow-up question of. ")
	b.WriteString("You hold the tools defined above")
	if len(indexed) > 0 {
		b.WriteString(", and these exist too — call `tools.load` with the names you need:\n\n")
		b.WriteString(tools.Index(manifestsOf(indexed)))
	} else {
		b.WriteString(", and nothing else.\n")
	}
	b.WriteString("\nThat is the complete list. There is no MCP server here. ")
	b.WriteString("If the instructions above name a tool that is on neither list, it does not exist for you: ")
	b.WriteString("use the closest one that does, and say in your answer what you could not do. Never guess a tool name.")
	return b.String()
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
