package agent

import (
	"context"
	"strings"
	"time"

	"github.com/MelloB1989/karmax/internal/tools"
	"github.com/MelloB1989/karmax/pkg/karmahelper"
	"go.uber.org/zap"
)

// Which tools ride in every request, and which are only named.
//
// The agent's job is to decide between acting and delegating. That decision
// needs a handful of tools in full and knowledge that the rest exist — not 71
// JSON schemas, which cost ~5,000 tokens per call and are mostly irrelevant to
// any one turn.

// defaultCoreTools are held in full because the agent reaches for them on
// ordinary turns, so indexing them would only buy a round trip.
var defaultCoreTools = []string{
	"comms.send",
	"claude_code.call",
	"subagent.spawn",
	"memory.retrieve",
	"memory.ingest",
	"profile.update",
	"scheduler.add",
	"whatsapp.read",
	"review.resolve",
	"comms.escalate",
	"self.remind",
	"karmax.capabilities",
	"tools.search",
}

// splitToolSet divides tools into those the session holds and those it only
// lists. An empty CoreTools keeps every tool in full, which is the pre-index
// behaviour and the escape hatch if indexing misbehaves.
func (a *Agent) splitToolSet(all []tools.Tool) (held, indexed []tools.Tool) {
	core := a.def.CoreTools
	if len(core) == 0 {
		core = defaultCoreTools
	}
	if len(core) == 1 && strings.TrimSpace(core[0]) == "*" {
		return all, nil
	}

	inCore := make(map[string]bool, len(core)*2)
	for _, n := range core {
		n = strings.TrimSpace(n)
		if n == "" {
			continue
		}
		inCore[n] = true
		inCore[tools.CanonicalName(n)] = true
	}

	for _, t := range all {
		name := t.Manifest().Name
		if inCore[name] || inCore[tools.CanonicalName(name)] {
			held = append(held, t)
			continue
		}
		indexed = append(indexed, t)
	}
	return held, indexed
}

// lendNamed resolves tool names against the agent's full set, for handing a
// follow-up turn the tools the model just asked for.
func (a *Agent) lendNamed(names []string) []tools.Tool {
	a.mu.RLock()
	all := a.allTools
	a.mu.RUnlock()

	want := make(map[string]bool, len(names)*2)
	for _, n := range names {
		n = strings.TrimSpace(n)
		if n == "" {
			continue
		}
		want[n] = true
		want[tools.CanonicalName(n)] = true
	}

	var out []tools.Tool
	for _, t := range all {
		name := t.Manifest().Name
		if want[name] || want[tools.CanonicalName(name)] {
			out = append(out, t)
		}
	}
	return out
}

func manifestsOf(ts []tools.Tool) []tools.ToolManifest {
	out := make([]tools.ToolManifest, 0, len(ts))
	for _, t := range ts {
		out = append(out, t.Manifest())
	}
	return out
}

func manifestNames(ts []tools.Tool) []string {
	out := make([]string, 0, len(ts))
	for _, t := range ts {
		out = append(out, t.Manifest().Name)
	}
	return out
}

// requestedTools pulls the names a tools.load call asked for out of this turn's
// records. Reading the records is what makes this possible at all — before tool
// calls were observable there was no way to see the request.
func requestedTools(calls []karmahelper.ToolCallRecord) []string {
	var out []string
	for _, c := range calls {
		if tools.CanonicalName(c.Name) != "tools_load" {
			continue
		}
		// The tool already resolved the names against the registry, so its
		// output is authoritative; the raw input is the fallback.
		if o, ok := c.Result.Output.(map[string]any); ok {
			if loaded, ok := o["loaded"].([]string); ok && len(loaded) > 0 {
				out = append(out, loaded...)
				continue
			}
		}
		if names, ok := c.Input["names"]; ok {
			out = append(out, toStringList(names)...)
		}
	}
	return out
}

func toStringList(v any) []string {
	switch vv := v.(type) {
	case []string:
		return vv
	case []any:
		var out []string
		for _, e := range vv {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
		return out
	case string:
		if strings.TrimSpace(vv) == "" {
			return nil
		}
		return []string{vv}
	}
	return nil
}

// maxToolLoadRounds bounds how many times one turn may stop to fetch tools. Two
// is enough for "ask, then act, then ask once more"; beyond that the model is
// deliberating rather than working, and it continues with what it holds.
const maxToolLoadRounds = 2

// loadRequestedTools finishes a turn the model paused to ask for a tool.
//
// It holds a core set and an index of everything else, so "I need X" is a real
// step rather than a failure. Shared by the event and chat paths: a capability
// that only appears on one of them is worse than not having it, because the
// model is told the tool exists and then cannot call it.
func (a *Agent) loadRequestedTools(ctx context.Context, session *MainModelSession,
	response string, calls []karmahelper.ToolCallRecord, withhold map[string]bool) (string, []karmahelper.ToolCallRecord) {

	for round := 0; round < maxToolLoadRounds; round++ {
		want := requestedTools(calls)
		if len(want) == 0 {
			return response, calls
		}

		// The loader honours the turn's withhold. It did not, and that was the
		// hole: an observe pass, finding no send tool in its set, simply ASKED
		// for comms.send by name and was handed it — the log reads "loading
		// tools the model asked for: comms.send" one line after the pass was
		// built without it. A capability removed from a turn must be removed
		// from every round of that turn, or removing it only adds a step.
		var grantable, refused []string
		for _, name := range want {
			if withhold[tools.CanonicalName(name)] {
				refused = append(refused, name)
			} else {
				grantable = append(grantable, name)
			}
		}
		if len(refused) > 0 {
			a.log.Info("refusing to lend withheld tools",
				zap.String("agent", a.def.ID), zap.Strings("tools", refused))
		}

		lent := a.lendNamed(grantable)
		if len(lent) == 0 && len(refused) == 0 {
			a.log.Warn("the model asked for tools this agent cannot bind",
				zap.String("agent", a.def.ID), zap.Strings("tools", want))
			return response, calls
		}

		followUp := "The tools you requested are now available. Use them to finish the task."
		if len(refused) > 0 {
			followUp = "Withheld on this pass, not loadable: " + strings.Join(refused, ", ") +
				". This pass cannot send messages or speak in any chat, and asking again will not change that. "
			if len(lent) > 0 {
				followUp += "The rest of what you asked for is available. Finish the task without the withheld tools."
			} else {
				followUp += "Finish the task without them and report what you did."
			}
		}

		a.log.Info("loading tools the model asked for",
			zap.String("agent", a.def.ID), zap.Strings("granted", grantable), zap.Strings("refused", refused))

		lctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
		resp2, tc2, err := session.ProcessMessageWithheld(lctx, followUp, lent, withhold)
		cancel()
		if err != nil || strings.TrimSpace(resp2) == "" {
			return response, calls
		}
		response, calls = cleanOutboundResponse(resp2), tc2
	}
	return response, calls
}

// NamedTools exposes specific bound tools to other subsystems.
//
// The voice brain answers as this agent and must write to the SAME memory —
// handing it the agent's own bound instances is what keeps one set of facts.
func (a *Agent) NamedTools(names ...string) []tools.Tool {
	return a.lendNamed(names)
}
