package builtin

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/MelloB1989/karmax/internal/tools"
)

// Google Workspace through gogcli, with a service account.
//
// The gws CLI it replaces could only do interactive browser OAuth, and this
// Workspace enforces a reauth (RAPT) policy — so an unattended daemon was
// logged out every few hours by design, and no amount of token handling would
// have changed that. A service account with domain-wide delegation never
// reauths, which is the only shape that fits a process nobody is sitting at.
//
// It also gets multi-account for free: a service account impersonates whichever
// user it is pointed at, so "which mailbox" becomes a parameter rather than a
// second login.

const gogTimeout = 90 * time.Second

// GogTool runs Google Workspace operations via the gog binary.
type GogTool struct {
	// Path to the gog binary. Empty resolves from PATH.
	Path string
	// DefaultAccount is impersonated when a call names none.
	DefaultAccount string
}

func (t *GogTool) Manifest() tools.ToolManifest {
	return tools.ToolManifest{
		Name: "google",
		Description: "Run Google Workspace operations — Gmail, Calendar, Drive, Docs, Sheets, Contacts, Tasks — via the gog CLI. " +
			"Pass the command as an argument list, e.g. [\"gmail\",\"ls\",\"--max\",\"5\"] or [\"calendar\",\"events\",\"list\",\"--today\"]. " +
			"Use 'account' to act as a specific user when the operator has more than one. " +
			"Output comes back as JSON. If you are unsure of a subcommand, run it with --help first rather than guessing.",
		Parameters: json.RawMessage(`{
			"type": "object",
			"properties": {
				"args": {
					"type": "array",
					"items": {"type": "string"},
					"description": "The gog command and flags, without the leading 'gog'. e.g. [\"gmail\",\"search\",\"from:siva\",\"--max\",\"10\"]"
				},
				"account": {"type": "string", "description": "Email or alias to act as. Omit for the default account."}
			},
			"required": ["args"]
		}`),
	}
}

func (t *GogTool) Execute(ctx context.Context, input map[string]any) (tools.ToolResult, error) {
	args := stringList(input["args"])
	if len(args) == 0 {
		return tools.ErrorResult(fmt.Errorf("give the gog command as args, e.g. [\"gmail\",\"ls\"]")), nil
	}

	account, _ := input["account"].(string)
	account = strings.TrimSpace(account)
	if account == "" {
		account = t.DefaultAccount
	}

	full := append([]string{}, args...)
	if account != "" && !hasFlag(args, "--account", "-a") {
		full = append(full, "--account", account)
	}
	// --no-input so a missing credential fails with a message instead of
	// blocking forever on a prompt nobody is there to answer; --json because
	// the caller is a model, not a terminal.
	if !hasFlag(args, "--no-input") {
		full = append(full, "--no-input")
	}
	if !hasFlag(args, "--json", "-j", "--plain", "-p", "--help", "-h") {
		full = append(full, "--json")
	}

	runCtx, cancel := context.WithTimeout(ctx, gogTimeout)
	defer cancel()

	cmd := exec.CommandContext(runCtx, t.bin(), full...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()

	out := stdout.String()
	if len(out) > maxOutputLen {
		out = out[:maxOutputLen] + "\n... [truncated]"
	}
	if err != nil {
		// stdout travels with the error: gog reports an auth problem in its
		// body, and that body is the whole diagnosis. Without it the agent can
		// only say "google failed" and the operator is left guessing which
		// command fixes it.
		msg := stderr.String()
		if msg == "" {
			msg = err.Error()
		}
		return tools.ToolResult{
			Output:  map[string]any{"output": out},
			Error:   fmt.Sprintf("gog %s: %s", strings.Join(args, " "), strings.TrimSpace(msg)),
			IsError: true,
		}, nil
	}
	return tools.SuccessResult(map[string]any{"output": out}), nil
}

func (t *GogTool) bin() string {
	if strings.TrimSpace(t.Path) != "" {
		return t.Path
	}
	if p, err := exec.LookPath("gog"); err == nil {
		return p
	}
	// The usual go install destination, for a daemon whose PATH is systemd's.
	if home, err := os.UserHomeDir(); err == nil {
		return home + "/go/bin/gog"
	}
	return "gog"
}

// hasFlag reports whether the caller already supplied one of these flags, so a
// deliberate choice is never overridden.
func hasFlag(args []string, flags ...string) bool {
	for _, a := range args {
		for _, f := range flags {
			if a == f || strings.HasPrefix(a, f+"=") {
				return true
			}
		}
	}
	return false
}
