// Package googleworkspace connects KARMAX to Google through the gog CLI.
//
// This is the second way Google reaches KARMAX and the two are not
// interchangeable, so it is worth being clear which is which:
//
//   - internal/connectors/google is PER EMPLOYEE. The org registers one OAuth
//     app, each person authorises their own account against it, and KARMAX
//     holds their tokens. That is the shape for an org.
//   - this one is PER MACHINE. gogcli holds its own session — KARMAX stores no
//     credential for it at all — and whoever is signed into gog on this
//     computer is who the agent acts as. That is the shape for a laptop.
//
// They keep separate ids (`google` and `google_workspace`) because they are
// separate things that can both be set up at once, and collapsing them would
// mean one silently replacing the other in the registry.
//
// # Why a connector rather than a builtin tool
//
// It used to be a builtin tool plus a hand-written health check in the
// integrations catalogue, which meant its manifest, its tools and its health
// lived in three files that had to be kept agreeing. As a connector it is one
// object, it is reachable from `karmax login`, and — the reason this move
// happened now — it obeys the `connectors:` allowlist in karmax.yaml like
// everything else, so an install that does not want Google can say so once.
package googleworkspace

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/MelloB1989/karmax/pkg/connectorkit"
)

// Connector is the gog CLI session on this machine.
type Connector struct {
	// Path to the gog binary. Empty resolves from PATH.
	Path string
	// Run executes one gog command, given the tool's own input map.
	//
	// Supplied by the runtime rather than implemented here, so that the flag
	// defaults (--no-input, --json, --account), the timeout, the output cap and
	// the exit-code reasons all keep living in one place —
	// internal/tools/builtin/gog.go, which already does them well. Copying that
	// logic into this package would mean two versions of it drifting apart.
	Run func(ctx context.Context, input map[string]any) (any, error)
}

func New(path string, run func(context.Context, map[string]any) (any, error)) *Connector {
	return &Connector{Path: path, Run: run}
}

func (c *Connector) Manifest() connectorkit.Manifest {
	return connectorkit.Manifest{
		ID: "google_workspace",
		// Named for the thing that holds the session, because an operator
		// choosing between this and the OAuth connector needs to see the
		// difference in the list rather than read both descriptions.
		Name: "Google (gog CLI on this machine)",
		Description: "Gmail, Calendar, Drive, Docs, Sheets, Contacts and Tasks through the gog CLI. " +
			"The session belongs to gog rather than to KARMAX, so signing in and out is done with " +
			"`gog auth`, and nothing here can be revoked by deleting a token from KARMAX.",
		Capabilities: []string{"exec:gog"},
	}
}

// Auth is a CLI session: gog holds it, KARMAX only asks whether it is there.
// There is nothing for the operator to paste, which is why Config is empty.
func (c *Connector) Auth() connectorkit.AuthMethod {
	return connectorkit.AuthMethod{Kind: connectorkit.AuthCLI}
}

func (c *Connector) Sources() []connectorkit.EventSource { return nil }

// Health asks gog which accounts it holds.
//
// `gog auth status` is not the check it looks like: it describes the keyring
// and exits 0 on a machine that has never authorized anybody. The question
// worth answering is whether there is an account to act AS, so this counts
// them and calls zero a failure.
func (c *Connector) Health(ctx context.Context, _ connectorkit.Credentials) error {
	bin := c.bin()
	if _, err := exec.LookPath(bin); err != nil {
		return fmt.Errorf("the gog CLI is not on this machine (%v)", err)
	}
	cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	out, err := exec.CommandContext(cctx, bin, "auth", "list", "--json", "--no-input").CombinedOutput()
	if err != nil {
		return fmt.Errorf("gog auth list: %v — %s", err, trunc(strings.TrimSpace(string(out)), 300))
	}
	var listed struct {
		Accounts []struct {
			Email string `json:"email"`
		} `json:"accounts"`
	}
	if err := json.Unmarshal(out, &listed); err != nil {
		return fmt.Errorf("gog auth list returned something unreadable: %v", err)
	}
	if len(listed.Accounts) == 0 {
		return fmt.Errorf("no Google account is authorized yet — run `gog auth add`")
	}
	return nil
}

func (c *Connector) Tools() []connectorkit.Tool {
	return []connectorkit.Tool{
		{
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
			Call: c.call,
		},
	}
}

func (c *Connector) call(ctx context.Context, _ connectorkit.Credentials, in map[string]any) (any, error) {
	if c.Run == nil {
		return nil, fmt.Errorf("google: this build cannot run gog commands")
	}
	return c.Run(ctx, in)
}

func (c *Connector) bin() string {
	if strings.TrimSpace(c.Path) != "" {
		return c.Path
	}
	return "gog"
}

// trunc keeps a CLI's complaint readable in an error line.
func trunc(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}
