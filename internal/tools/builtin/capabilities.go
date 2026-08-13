package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/MelloB1989/karmax/internal/store"
	"github.com/MelloB1989/karmax/internal/tools"
)

// Knowing what it can do.
//
// The agent holds a handful of tools in full and an index of the rest, runs
// loops it never sees listed, and has recipes it can now write — but nothing
// told it any of that. An agent that cannot enumerate its own capabilities
// declines things it is perfectly able to do, which is the failure the operator
// notices as "it says it can't, but it can".

// LoopLister reports the loops this instance runs. Supplied by the runtime,
// which owns them.
type LoopLister func() []LoopInfo

// LoopInfo is one loop and its health.
type LoopInfo struct {
	Name     string `json:"name"`
	Triggers string `json:"triggers,omitempty"`
	Running  bool   `json:"running,omitempty"`
	Dark     bool   `json:"dark,omitempty"`
	LastErr  string `json:"last_error,omitempty"`
}

// CapabilitiesTool answers "what can I do, and what is running".
type CapabilitiesTool struct {
	Registry *tools.Registry
	Store    *store.Store
	AgentID  string
	Loops    LoopLister
	// Held names the tools carried in full this turn, so the answer can
	// distinguish those from the ones needing tools.load first.
	Held []string
	// Missing names tools the config asked for that nothing answers to. An
	// agent that cannot see its own gaps guesses at names to fill them.
	Missing []string
	// Loadable names the tools this agent can fetch with tools.load — its own
	// indexed set, NOT everything on the instance. Reporting the registry here
	// told the agent it could load tools that tools.load cannot bind, because
	// binding resolves against the agent's configured toolset.
	Loadable []string
}

func (t *CapabilitiesTool) Manifest() tools.ToolManifest {
	return tools.ToolManifest{
		Name: "karmax.capabilities",
		Description: "List what you can actually do: your tools (which you hold and which you must load first), " +
			"the loops and recipes running on this instance, and any that have gone quiet. " +
			"Check here before telling the operator you cannot do something — you may have the tool and not be holding it.",
		Parameters: json.RawMessage(`{
			"type": "object",
			"properties": {
				"section": {"type": "string", "enum": ["all", "tools", "loops", "subagents"], "description": "Defaults to all."}
			}
		}`),
	}
}

func (t *CapabilitiesTool) Execute(ctx context.Context, input map[string]any) (tools.ToolResult, error) {
	section, _ := input["section"].(string)
	section = strings.ToLower(strings.TrimSpace(section))
	if section == "" || section == "<nil>" {
		section = "all"
	}
	out := map[string]any{}

	if section == "all" || section == "tools" {
		held := map[string]bool{}
		for _, n := range t.Held {
			held[n] = true
		}
		haveNow := append([]string(nil), t.Held...)
		canLoad := append([]string(nil), t.Loadable...)
		mine := map[string]bool{}
		for _, n := range append(append([]string{}, haveNow...), canLoad...) {
			mine[n] = true
		}
		// Present on this instance but not granted to this agent. A real case:
		// LinkedIn was connected, linkedin.post was registered, and the agent
		// told the operator it had no LinkedIn integration — true for the agent,
		// and unfixable by it, because the tool was never in its config. Naming
		// the difference turns "I cannot" into "grant me this and I can".
		var ungranted []string
		if t.Registry != nil {
			for _, m := range t.Registry.List() {
				if !mine[m.Name] {
					ungranted = append(ungranted, m.Name)
				}
			}
		}
		sort.Strings(haveNow)
		sort.Strings(canLoad)
		sort.Strings(ungranted)
		out["tools_held"] = haveNow
		out["tools_loadable"] = canLoad
		out["how_to_load"] = "call tools.load with the names you need"
		if len(ungranted) > 0 {
			out["tools_on_this_instance_but_not_yours"] = ungranted
			out["ungranted_note"] = "you cannot call these yourself and tools.load will not fetch them — " +
				"but you can HAND ONE TO A SUB-AGENT: subagent.spawn takes a 'tools' list, and a child built " +
				"around a tool can use it. That is the intended route, not a workaround. Never tell the " +
				"operator KARMAX lacks the capability, and do not ask them to edit karmax.yaml — spawn the " +
				"sub-agent and do the work"
		}
		if len(t.Missing) > 0 {
			out["tools_configured_but_missing"] = t.Missing
			out["missing_note"] = "these names are in your config but no tool answers to them — " +
				"treat them as unavailable and tell the operator rather than looking for a substitute name"
		}
	}

	if section == "all" || section == "loops" {
		if t.Loops != nil {
			loops := t.Loops()
			out["loops"] = loops
			var dark []string
			for _, l := range loops {
				if l.Dark {
					dark = append(dark, l.Name)
				}
			}
			if len(dark) > 0 {
				out["loops_gone_quiet"] = dark
			}
		}
		out["recipes_note"] = "call recipe.write with action 'list' to see recipes, or write a new one"
	}

	if (section == "all" || section == "subagents") && t.Store != nil {
		if runs, err := t.Store.SubagentRuns(t.AgentID, 5); err == nil && len(runs) > 0 {
			recent := make([]map[string]any, 0, len(runs))
			for _, r := range runs {
				recent = append(recent, map[string]any{
					"task": truncate(r.Task, 100), "status": r.Status,
				})
			}
			out["recent_subagents"] = recent
		}
		if n, err := t.Store.RunningSubagents(t.AgentID); err == nil {
			out["subagents_running"] = n
		}
	}

	if len(out) == 0 {
		return tools.ErrorResult(fmt.Errorf("unknown section %q", section)), nil
	}
	return tools.SuccessResult(out), nil
}
