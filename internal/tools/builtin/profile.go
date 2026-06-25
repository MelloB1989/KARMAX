package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/MelloB1989/karmax/internal/memory"
	"github.com/MelloB1989/karmax/internal/tools"
)

// ProfileTool lets the agent read and maintain a single curated Markdown
// document about the operator (ABOUT_ME.md). This is the agent's living,
// deduplicated understanding of who the user is — identity, projects,
// preferences, relationships, goals — distinct from the append-only memory log.
type ProfileTool struct {
	MemoryMgr *memory.Manager // set per-agent at registration time
	AgentID   string
}

func (t *ProfileTool) Manifest() tools.ToolManifest {
	return tools.ToolManifest{
		Name: "profile.update",
		Description: "Read or rewrite the curated 'about me' profile (ABOUT_ME.md): the living, deduplicated summary of who the operator is — identity, projects, preferences, relationships, and goals. " +
			"Use action 'read' to fetch the current profile, then action 'write' with the FULL new Markdown to replace it. Always read before writing and preserve facts that are still true; only revise what changed.",
		Parameters: json.RawMessage(`{
			"type": "object",
			"properties": {
				"action": {"type": "string", "enum": ["read", "write"], "description": "'read' returns the current profile; 'write' overwrites it with 'content'."},
				"content": {"type": "string", "description": "For action 'write': the full new Markdown profile document. Replaces the existing one entirely."}
			},
			"required": ["action"]
		}`),
	}
}

func (t *ProfileTool) Execute(ctx context.Context, input map[string]any) (tools.ToolResult, error) {
	if t.MemoryMgr == nil {
		return tools.ErrorResult(fmt.Errorf("memory manager not configured")), nil
	}

	action := strings.ToLower(strings.TrimSpace(fmt.Sprintf("%v", input["action"])))
	switch action {
	case "read", "", "<nil>":
		current, err := t.MemoryMgr.ReadProfile()
		if err != nil {
			return tools.ErrorResult(fmt.Errorf("read profile: %w", err)), nil
		}
		if strings.TrimSpace(current) == "" {
			current = "(profile is empty — no ABOUT_ME.md yet; write one to start)"
		}
		return tools.SuccessResult(map[string]any{
			"path":    t.MemoryMgr.ProfilePath(),
			"profile": current,
		}), nil

	case "write":
		content, _ := input["content"].(string)
		if strings.TrimSpace(content) == "" {
			return tools.ErrorResult(fmt.Errorf("content is required for action 'write'")), nil
		}
		stamped := ensureProfileHeader(content)
		if err := t.MemoryMgr.WriteProfile(stamped); err != nil {
			return tools.ErrorResult(fmt.Errorf("write profile: %w", err)), nil
		}
		return tools.SuccessResult(map[string]any{
			"path":   t.MemoryMgr.ProfilePath(),
			"status": "profile updated",
			"bytes":  len(stamped),
		}), nil

	default:
		return tools.ErrorResult(fmt.Errorf("unknown action %q (use 'read' or 'write')", action)), nil
	}
}

// ensureProfileHeader stamps the document with a machine-readable last-updated
// marker, replacing any existing stamp on the first line.
func ensureProfileHeader(content string) string {
	content = strings.TrimSpace(content)
	stamp := fmt.Sprintf("<!-- last updated: %s -->", time.Now().UTC().Format(time.RFC3339))

	if strings.HasPrefix(content, "<!-- last updated:") {
		if idx := strings.Index(content, "\n"); idx >= 0 {
			content = strings.TrimSpace(content[idx+1:])
		} else {
			content = ""
		}
	}

	if content == "" {
		return stamp + "\n"
	}
	return stamp + "\n\n" + content + "\n"
}
