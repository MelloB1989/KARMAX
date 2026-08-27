// Package slack exposes the Slack workspace as a connector.
//
// Slack was already wired into KARMAX as a COMMS CHANNEL — the thing that
// receives mentions and replies in a thread. That is a different subsystem from
// connectors, which is why Slack never appeared on the console's Connectors
// page despite obviously being connected: the page lists connectors, and Slack
// was not one.
//
// This does not replace the channel. The channel remains how conversation
// happens; this makes the workspace visible where an operator looks for it,
// gives it a health check, and adds the few tools that are about the workspace
// rather than about a conversation — posting somewhere the agent was not
// spoken to, and looking a person up.
package slack

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/MelloB1989/karmax/pkg/connectorkit"
)

var api = "https://slack.com/api"

var httpClient = &http.Client{Timeout: 30 * time.Second}

// Connector is one Slack workspace.
type Connector struct{}

func New() *Connector { return &Connector{} }

func (c *Connector) Manifest() connectorkit.Manifest {
	return connectorkit.Manifest{
		ID:   "slack",
		Name: "Slack",
		Description: "The workspace the agent talks in. Conversation itself is handled by the comms " +
			"channel; these tools are for posting somewhere nobody asked and for looking people up.",
		Capabilities: []string{"http:slack.com"},
		Config: []connectorkit.ConfigField{
			{Key: "bot_token", Description: "Bot user OAuth token (xoxb-…), from OAuth & Permissions", Required: true, Secret: true},
			{Key: "app_token", Description: "App-level token (xapp-…) with connections:write, from Basic Information — needed for Socket Mode", Secret: true},
			{Key: "signing_secret", Description: "Signing secret, from Basic Information — verifies inbound requests", Secret: true},
			{Key: "default_channel", Description: "Channel id used when a post omits one, e.g. C0123456789"},
		},
	}
}

func (c *Connector) Auth() connectorkit.AuthMethod {
	return connectorkit.AuthMethod{Kind: connectorkit.AuthAPIKey, APIKeyField: "bot_token"}
}

// botToken resolves the token, falling back to the environment.
//
// The daemon has always read SLACK_BOT_TOKEN from its .env, and that is where
// this install's token still lives. Reading it here means the connector reports
// the truth — connected — rather than "not configured" next to a bot that is
// visibly answering in Slack. A console-saved credential wins, so configuring
// it properly later migrates cleanly.
func botToken(cr connectorkit.Credentials) string {
	if t := strings.TrimSpace(cr.Get("bot_token")); t != "" {
		return t
	}
	if t := strings.TrimSpace(cr.AccessToken); t != "" {
		return t
	}
	return strings.TrimSpace(os.Getenv("SLACK_BOT_TOKEN"))
}

func appToken(cr connectorkit.Credentials) string {
	if t := strings.TrimSpace(cr.Get("app_token")); t != "" {
		return t
	}
	return strings.TrimSpace(os.Getenv("SLACK_APP_TOKEN"))
}

// call makes one Slack Web API request.
//
// Slack answers errors with HTTP 200 and {"ok":false,"error":"..."}, so the
// status code says almost nothing and the body has to be checked every time.
func call(ctx context.Context, token, method string, form url.Values, out any) error {
	if token == "" {
		return fmt.Errorf("slack: no bot token configured")
	}
	if form == nil {
		form = url.Values{}
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, api+"/"+method,
		strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded; charset=utf-8")

	res, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()

	var envelope struct {
		OK     bool            `json:"ok"`
		Error  string          `json:"error"`
		Needed string          `json:"needed"`
		Raw    json.RawMessage `json:"-"`
	}
	raw, err := readAll(res)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return fmt.Errorf("slack: %s returned something that is not JSON", method)
	}
	if !envelope.OK {
		return slackError(method, envelope.Error, envelope.Needed)
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(raw, out)
}

// slackError translates the codes an operator will actually hit.
func slackError(method, code, needed string) error {
	switch code {
	case "invalid_auth", "token_revoked", "account_inactive":
		return fmt.Errorf("slack: the bot token was rejected (%s) — regenerate it in OAuth & Permissions", code)
	case "missing_scope":
		return fmt.Errorf("slack: the bot token is missing the %q scope — add it in OAuth & Permissions "+
			"and REINSTALL the app, since scopes only take effect on reinstall", needed)
	case "not_in_channel":
		return fmt.Errorf("slack: the bot is not in that channel — invite it with /invite @your-bot")
	case "channel_not_found":
		return fmt.Errorf("slack: no such channel — use a channel id (C…) rather than a name, " +
			"and note the bot cannot see channels it has not been invited to")
	default:
		return fmt.Errorf("slack: %s failed: %s", method, code)
	}
}

func (c *Connector) Health(ctx context.Context, cr connectorkit.Credentials) error {
	cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()

	var me struct {
		User string `json:"user"`
		Team string `json:"team"`
	}
	if err := call(cctx, botToken(cr), "auth.test", nil, &me); err != nil {
		return err
	}

	// Socket Mode is how this install receives messages, and its token is
	// separate. A bot token that works while the app token is missing looks
	// healthy and cannot hear anything, which is the exact failure this install
	// spent a day on.
	if appToken(cr) == "" {
		return fmt.Errorf("slack: connected as %s to %s, but no app-level token (xapp-…) is set — "+
			"the bot can post and cannot receive; generate one in Basic Information → App-Level "+
			"Tokens with connections:write", me.User, me.Team)
	}
	return nil
}

func (c *Connector) Sources() []connectorkit.EventSource { return nil }
