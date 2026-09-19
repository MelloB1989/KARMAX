package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/MelloB1989/karmax/internal/browser"
	"github.com/MelloB1989/karmax/internal/tools"
)

// The browser, as something the agent can point at a page — and, past
// open/status/start, as the generic network-inspection surface a harness
// uses to discover and replay a site's own API instead of scripting a
// browser through it by hand (tabs/requests/request/fetch/eval). One tool
// name throughout: this file is also what POST /api/tools/browser and the
// `karmax browser` CLI subcommands both reach, so the action names and their
// parameters here ARE the contract those two speak.
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
			"'status' says whether it is running and what is open. 'start' opens it without navigating anywhere. 'stop' closes it (their sign-ins are kept). " +
			"'tabs' lists open tabs with their ids. 'requests' lists captured network requests (filterable by tab/url/method/type/status). " +
			"'request' returns one captured request in full, including its response body when one was kept. " +
			"'fetch' replays a request from inside a tab via its own fetch(), so the tab's cookies apply — pass 'from' to copy a captured request's method/headers/body, or build one from scratch. " +
			"'eval' runs JavaScript in a tab, awaiting any returned promise, and gives back the JSON result.",
		Parameters: json.RawMessage(`{
            "type": "object",
            "properties": {
                "action": {"type": "string", "enum": ["open", "status", "start", "stop", "tabs", "requests", "request", "fetch", "eval"], "description": "What to do. Defaults to 'status'."},
                "url": {"type": "string", "description": "The page to put in front of them, for 'open'. The URL to request, for 'fetch'."},
                "tab": {"type": "string", "description": "A tab id, for 'requests'/'fetch'/'eval'. Defaults to the most recently active tab (or, for 'fetch' with 'from', that request's own tab)."},
                "id": {"type": "string", "description": "A captured request id, for 'request'."},
                "url_contains": {"type": "string", "description": "Substring filter on URL, for 'requests'."},
                "method": {"type": "string", "description": "Filter by HTTP method, for 'requests'. The method to send, for 'fetch' (default GET, or the 'from' request's method)."},
                "type": {"type": "string", "enum": ["xhr", "fetch", "document", "other"], "description": "Filter by resource type, for 'requests'."},
                "status": {"type": "integer", "description": "Filter by exact HTTP status code, for 'requests'."},
                "limit": {"type": "integer", "description": "Max results for 'requests'. Default 50, newest first."},
                "from": {"type": "string", "description": "For 'fetch': a captured request id whose method/headers/body to reuse (Cookie/Host/Content-Length excluded — the tab supplies those itself), unless overridden."},
                "headers": {"type": "array", "items": {"type": "string"}, "description": "Extra 'Key: Value' headers for 'fetch', applied on top of anything copied via 'from'."},
                "body": {"type": "string", "description": "Request body for 'fetch'."},
                "js": {"type": "string", "description": "JavaScript to evaluate in the tab, for 'eval'."}
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

	case "stop":
		if err := t.Session.Stop(ctx); err != nil {
			return tools.ErrorResult(err), nil
		}
		return tools.SuccessResult(map[string]any{"running": false}), nil

	case "tabs":
		tabs, err := t.Session.Tabs(ctx)
		if err != nil {
			return tools.ErrorResult(err), nil
		}
		out := make([]map[string]any, 0, len(tabs))
		for _, tab := range tabs {
			out = append(out, map[string]any{"id": tab.ID, "title": tab.Title, "url": tab.URL})
		}
		return tools.SuccessResult(map[string]any{"tabs": out}), nil

	case "requests":
		filter := browser.RequestFilter{
			TabID:        str(input["tab"]),
			URLContains:  str(input["url_contains"]),
			Method:       str(input["method"]),
			ResourceType: str(input["type"]),
			Limit:        50,
		}
		if v, ok := asInt(input["status"]); ok {
			filter.Status = v
		}
		if v, ok := asInt(input["limit"]); ok && v > 0 {
			filter.Limit = v
		}
		return tools.SuccessResult(map[string]any{"requests": t.Session.Requests(filter)}), nil

	case "request":
		id := str(input["id"])
		if id == "" {
			return tools.ErrorResult(fmt.Errorf("id is required")), nil
		}
		rec, ok := t.Session.RequestByID(id)
		if !ok {
			return tools.ErrorResult(fmt.Errorf("no captured request %q", id)), nil
		}
		return tools.SuccessResult(rec), nil

	case "fetch":
		url := str(input["url"])
		method := str(input["method"])
		tabID := str(input["tab"])
		headers := map[string]string{}
		var body string
		var hasBody bool

		if from := str(input["from"]); from != "" {
			rec, ok := t.Session.RequestByID(from)
			if !ok {
				return tools.ErrorResult(fmt.Errorf("no captured request %q to copy from", from)), nil
			}
			if url == "" {
				url = rec.URL
			}
			if tabID == "" {
				tabID = rec.TabID
			}
			if method == "" {
				method = rec.Method
			}
			for k, v := range browser.FilterReplayHeaders(rec.RequestHeaders) {
				headers[k] = v
			}
			if rec.RequestBody != "" {
				body, hasBody = rec.RequestBody, true
			}
		}
		for _, line := range asStrList(input["headers"]) {
			k, v, found := strings.Cut(line, ":")
			if !found {
				continue
			}
			headers[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
		// An explicit "" body still counts as an override — presence of the
		// key is what matters, not whether it is empty.
		if b, present := input["body"]; present {
			if s, ok := b.(string); ok {
				body, hasBody = s, true
			}
		}
		if url == "" {
			return tools.ErrorResult(fmt.Errorf("url is required (or 'from' a captured request)")), nil
		}
		tabID, err := t.resolveTab(ctx, tabID)
		if err != nil {
			return tools.ErrorResult(err), nil
		}
		res, err := t.Session.FetchInTab(ctx, tabID, browser.FetchSpec{
			URL: url, Method: method, Headers: headers, Body: body, HasBody: hasBody,
		})
		if err != nil {
			return tools.ErrorResult(err), nil
		}
		return tools.SuccessResult(map[string]any{
			"status": res.Status, "headers": res.Headers, "body": res.Body,
		}), nil

	case "eval":
		js := str(input["js"])
		if js == "" {
			return tools.ErrorResult(fmt.Errorf("js is required")), nil
		}
		tabID, err := t.resolveTab(ctx, str(input["tab"]))
		if err != nil {
			return tools.ErrorResult(err), nil
		}
		raw, err := t.Session.Eval(ctx, tabID, js)
		if err != nil {
			return tools.ErrorResult(err), nil
		}
		var result any
		if err := json.Unmarshal(raw, &result); err != nil {
			result = string(raw)
		}
		return tools.SuccessResult(map[string]any{"result": result}), nil

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

// resolveTab returns requested when it is non-empty, else the most recently
// active tab. Chrome's own /json/list ordering puts that tab first in
// practice — there is no separate "active tab" query on the DevTools HTTP
// surface to ask instead.
func (t *BrowserTool) resolveTab(ctx context.Context, requested string) (string, error) {
	if requested != "" {
		return requested, nil
	}
	tabs, err := t.Session.Tabs(ctx)
	if err != nil {
		return "", err
	}
	if len(tabs) == 0 {
		return "", fmt.Errorf("no open tabs")
	}
	return tabs[0].ID, nil
}

func asInt(v any) (int, bool) {
	switch n := v.(type) {
	case float64:
		return int(n), true
	case int:
		return n, true
	case string:
		if i, err := strconv.Atoi(strings.TrimSpace(n)); err == nil {
			return i, true
		}
	}
	return 0, false
}

func asStrList(v any) []string {
	arr, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(arr))
	for _, e := range arr {
		if s, ok := e.(string); ok && s != "" {
			out = append(out, s)
		}
	}
	return out
}
