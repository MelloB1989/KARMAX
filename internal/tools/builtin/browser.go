package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/MelloB1989/karmax/internal/browser"
	"github.com/MelloB1989/karmax/internal/tools"
)

// The browser, as something the agent can point at a page.
//
// Not a way to read the web — that is the Playwright MCP server, which attaches
// to the same window and is a far better instrument for it. This is the other
// half: the agent needs to be able to say "I have opened the sign-in page, it
// is in front of you now", because every OAuth flow reaches a step only a
// person can complete, and an agent that cannot hand over at that step just
// stalls.
type BrowserTool struct {
	Session *browser.Session
}

func (t *BrowserTool) Manifest() tools.ToolManifest {
	return tools.ToolManifest{
		Name: "browser",
		Description: "The operator's browser — one window they and you share, signed into whatever they have signed into. " +
			"'open' puts a URL in front of them and raises the window (use this whenever a flow needs them to sign in or approve something, then tell them what to do there). " +
			"'status' says whether it is running and what is open. 'start' opens it without navigating anywhere.",
		Parameters: json.RawMessage(`{
            "type": "object",
            "properties": {
                "action": {"type": "string", "enum": ["open", "status", "start"], "description": "What to do. Defaults to 'status'."},
                "url": {"type": "string", "description": "The page to put in front of them, for 'open'."}
            }
        }`),
	}
}

func (t *BrowserTool) Execute(ctx context.Context, input map[string]any) (tools.ToolResult, error) {
	if t.Session == nil {
		return tools.ErrorResult(fmt.Errorf("no browser is configured on this instance")), nil
	}
	action, _ := input["action"].(string)
	switch strings.ToLower(strings.TrimSpace(action)) {
	case "open":
		url, _ := input["url"].(string)
		tab, err := t.Session.Open(ctx, url)
		if err != nil {
			return tools.ErrorResult(err), nil
		}
		return tools.SuccessResult(map[string]any{
			"opened": tab.URL,
			"title":  tab.Title,
			"note":   "It is on their screen now. Tell them what to do there before you wait for it.",
		}), nil

	case "start":
		if err := t.Session.Start(ctx); err != nil {
			return tools.ErrorResult(err), nil
		}
		return tools.SuccessResult(map[string]any{"running": true, "profile": t.Session.Profile()}), nil

	default:
		running := t.Session.Running(ctx)
		out := map[string]any{"running": running, "profile": t.Session.Profile()}
		if running {
			if tabs, err := t.Session.Tabs(ctx); err == nil {
				open := make([]map[string]string, 0, len(tabs))
				for _, tab := range tabs {
					open = append(open, map[string]string{"title": tab.Title, "url": tab.URL})
				}
				out["tabs"] = open
			}
		}
		return tools.SuccessResult(out), nil
	}
}
