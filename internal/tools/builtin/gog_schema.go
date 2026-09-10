package builtin

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"

	"github.com/MelloB1989/karmax/internal/tools"
)

// GogSchemaTool answers "what flags does this Google command take" without
// spending a turn on --help.
//
// gogcli publishes a machine-readable contract per command path, which is a
// better thing to hand a model than help text: it names the flags, their types
// and the exit codes, and it does not change shape when the CLI reformats its
// output.
type GogSchemaTool struct {
	// Path to the gog binary. Empty resolves the same way GogTool does.
	Path string
}

func (t *GogSchemaTool) Manifest() tools.ToolManifest {
	return tools.ToolManifest{
		Name: "google.schema",
		Description: "Look up the exact contract for a Google (gog) command — its flags, arguments and output — before calling it. " +
			"Give the command path as a list, e.g. [\"calendar\",\"events\",\"list\"] or [\"gmail\",\"send\"]. " +
			"Omit it to get the whole CLI, which is large; prefer naming the command you mean.",
		Parameters: json.RawMessage(`{
            "type": "object",
            "properties": {
                "command": {
                    "type": "array",
                    "items": {"type": "string"},
                    "description": "Command path without the leading 'gog', e.g. [\"drive\",\"search\"]."
                }
            }
        }`),
	}
}

func (t *GogSchemaTool) Execute(ctx context.Context, input map[string]any) (tools.ToolResult, error) {
	command := stringList(input["command"])
	// A string is accepted too, because a model that has seen the old
	// service.resource.method form will send one.
	if len(command) == 0 {
		if s, _ := input["command"].(string); strings.TrimSpace(s) != "" {
			command = strings.FieldsFunc(s, func(r rune) bool { return r == '.' || r == ' ' })
		}
	}

	runCtx, cancel := context.WithTimeout(ctx, gogTimeout)
	defer cancel()

	cmd := exec.CommandContext(runCtx, (&GogTool{Path: t.Path}).bin(), append([]string{"schema"}, command...)...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return tools.ErrorResult(fmt.Errorf("gog schema %s: %s", strings.Join(command, " "), msg)), nil
	}

	out := stdout.String()
	if len(out) > maxOutputLen {
		out = out[:maxOutputLen] + "\n... [truncated — name a more specific command]"
	}
	return tools.SuccessResult(map[string]any{"schema": out}), nil
}
