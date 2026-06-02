package builtin

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/MelloB1989/karmax/internal/tools"
)

var defaultAllowedCommands = []string{
	"ls", "cat", "head", "tail", "wc", "grep", "find", "echo", "date",
	"curl", "wget", "jq", "yq", "git", "go", "npm", "node", "python3",
	"docker", "kubectl", "gh",
}

type ShellTool struct {
	AllowList []string
}

func (t *ShellTool) Manifest() tools.ToolManifest {
	return tools.ToolManifest{
		Name:        "shell.exec",
		Description: "Execute a shell command (allowlist-gated for safety)",
		Parameters: json.RawMessage(`{
			"type": "object",
			"properties": {
				"command": {"type": "string"},
				"args": {"type": "array", "items": {"type": "string"}},
				"timeout_ms": {"type": "integer"},
				"env": {"type": "object", "additionalProperties": {"type": "string"}}
			},
			"required": ["command"]
		}`),
	}
}

func (t *ShellTool) Execute(ctx context.Context, input map[string]any) (tools.ToolResult, error) {
	command, _ := input["command"].(string)
	if command == "" {
		return tools.ErrorResult(fmt.Errorf("command is required")), nil
	}

	base := strings.Fields(command)[0]
	allowed := t.AllowList
	if len(allowed) == 0 {
		allowed = defaultAllowedCommands
	}

	isAllowed := false
	for _, a := range allowed {
		if a == base {
			isAllowed = true
			break
		}
	}
	if !isAllowed {
		return tools.ErrorResult(fmt.Errorf("command %q is not in the allowlist", base)), nil
	}

	timeout := 30000
	if ms, ok := input["timeout_ms"].(float64); ok {
		timeout = int(ms)
	}

	timeoutCtx, cancel := context.WithTimeout(ctx, time.Duration(timeout)*time.Millisecond)
	defer cancel()

	var args []string
	if a, ok := input["args"].([]any); ok {
		for _, v := range a {
			if s, ok := v.(string); ok {
				args = append(args, s)
			}
		}
	}

	cmd := exec.CommandContext(timeoutCtx, "sh", "-c", command)
	if len(args) > 0 {
		cmd = exec.CommandContext(timeoutCtx, command, args...)
	}

	if envMap, ok := input["env"].(map[string]any); ok {
		for k, v := range envMap {
			if s, ok := v.(string); ok {
				cmd.Env = append(cmd.Env, k+"="+s)
			}
		}
	}

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	exitCode := 0
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		} else {
			return tools.ErrorResult(err), nil
		}
	}

	return tools.SuccessResult(map[string]any{
		"stdout":    stdout.String(),
		"stderr":    stderr.String(),
		"exit_code": exitCode,
	}), nil
}
