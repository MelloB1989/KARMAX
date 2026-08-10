package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/MelloB1989/karma/ai"
	"github.com/MelloB1989/karmax/internal/safety"
	"github.com/MelloB1989/karmax/internal/tools"
)

// Adopting tools a library already publishes.
//
// karma's ai.GoFunctionTool is what wacli (and increasingly anything else in
// the suite) exposes its capabilities as. KARMAX's registry deals in
// tools.Tool, and karmahelper already converts one way — this is the other
// direction, which is what lets an integration ship its own tools instead of
// KARMAX hand-writing a wrapper per API call and falling behind the moment the
// library gains a feature.
//
// Once adapted they are indistinguishable from a built-in: the agent gets them,
// a WASM workflow reaches them through the generic `tool` host function, and the
// Broker gates them by name. None of that needed changing, because tools were
// already the currency.

// FromGoFunctionTool adapts one karma tool into KARMAX's registry.
func FromGoFunctionTool(t ai.GoFunctionTool) tools.Tool {
	return &goFunctionTool{inner: t}
}

// FromGoFunctionTools adapts a set, which is how a library publishes them.
func FromGoFunctionTools(in []ai.GoFunctionTool) []tools.Tool {
	out := make([]tools.Tool, 0, len(in))
	for _, t := range in {
		out = append(out, FromGoFunctionTool(t))
	}
	return out
}

type goFunctionTool struct {
	inner ai.GoFunctionTool
	// fenceAs is the source label for tools returning other people's words. Set
	// by Fenced; empty means the output is ours and needs no fence.
	fenceAs string
}

// Fenced marks a tool whose output is content KARMAX did not write.
//
// Applied here rather than at each call site so a tool cannot arrive unfenced
// by being forgotten — which is the failure this exists to prevent, since the
// library will gain tools that this repo never explicitly lists.
func Fenced(t tools.Tool, source string) tools.Tool {
	if g, ok := t.(*goFunctionTool); ok {
		copy := *g
		copy.fenceAs = source
		return &copy
	}
	return t
}

func (g *goFunctionTool) Manifest() tools.ToolManifest {
	params := json.RawMessage(`{"type":"object","properties":{}}`)
	if g.inner.Parameters != nil {
		if raw, err := json.Marshal(g.inner.Parameters); err == nil {
			params = raw
		}
	}
	return tools.ToolManifest{
		Name:        g.inner.Name,
		Description: g.inner.Description,
		Parameters:  params,
	}
}

func (g *goFunctionTool) Execute(ctx context.Context, input map[string]any) (tools.ToolResult, error) {
	if g.inner.Handler == nil {
		return tools.ErrorResult(fmt.Errorf("%s has no implementation", g.inner.Name)), nil
	}
	params := ai.FuncParams{}
	for k, v := range input {
		params[k] = v
	}

	out, err := g.inner.Handler(ctx, params)
	if err != nil {
		// Surfaced to the model rather than aborting: these handlers return an
		// error only for a malformed request, and the model can fix that.
		return tools.ErrorResult(err), nil
	}
	if g.fenceAs != "" {
		out = safety.Fence(g.fenceAs, out)
	}
	return tools.SuccessResult(map[string]any{"raw": out}), nil
}

// fencedPrefixes name the tools whose output contains what other people wrote.
//
// A prefix list rather than an exact one: wacli will gain tools, and a new
// `whatsapp_search_*` arriving unfenced because nobody updated a list here is
// exactly the mistake that would not be noticed until a message told the agent
// to do something and it did.
var fencedPrefixes = []string{
	"whatsapp_search", "whatsapp_list_chats", "whatsapp_get_chat",
	"whatsapp_message", "whatsapp_download", "whatsapp_list_contacts",
	"whatsapp_get_contact", "whatsapp_resolve",
}

// FenceUntrusted marks the tools in a set whose output is other people's words.
func FenceUntrusted(in []tools.Tool, source string) []tools.Tool {
	out := make([]tools.Tool, 0, len(in))
	for _, t := range in {
		name := t.Manifest().Name
		fenced := false
		for _, p := range fencedPrefixes {
			if strings.HasPrefix(name, p) {
				fenced = true
				break
			}
		}
		if fenced {
			out = append(out, Fenced(t, source))
			continue
		}
		out = append(out, t)
	}
	return out
}
