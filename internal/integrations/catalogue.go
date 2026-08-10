// Package integrations is the catalogue: every provider KARMAX can connect to,
// described once so the daemon and the CLI agree about what exists.
//
// Kept separate from internal/integration (the mechanism) so that adding a
// provider is editing a list, not touching the auth layer. Both the running
// daemon and `karmax login` build the registry from here, which is what stops
// the CLI offering to connect something the daemon has never heard of.
package integrations

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/MelloB1989/karmax/internal/config"
	"github.com/MelloB1989/karmax/internal/hostpaths"
	"github.com/MelloB1989/karmax/internal/integration"
	"github.com/MelloB1989/karmax/internal/store"
	"github.com/MelloB1989/karmax/pkg/connectorkit"
)

// Build returns the registry for this instance's configuration.
func Build(cfg *config.KarmaxConfig, db *store.Store) *integration.Registry {
	resolver := integration.NewResolver(db, configLookup(cfg))
	reg := integration.NewRegistry(resolver)

	// The host-binary integrations. Registered whether or not a channel is
	// configured for them, because "is my WhatsApp still paired" is a question
	// worth answering even when nothing is using it yet.
	reg.Register(whatsApp())
	reg.Register(googleWorkspace())

	// The token-based channels, one per configured channel so two Slack
	// workspaces are two integrations rather than one that silently wins.
	for _, ch := range cfg.Comms.Channels {
		switch strings.ToLower(ch.Type) {
		case "discord":
			reg.Register(discord(ch.ID))
		case "slack":
			reg.Register(slack(ch.ID))
		case "telegram":
			reg.Register(telegram(ch.ID))
		}
	}
	return reg
}

// configLookup exposes karmax.yaml to the resolver.
//
// A channel's token lives at the top level and the rest under settings:, and
// both are already ${ENV}-interpolated by the config loader — so this only has
// to flatten them into one map.
func configLookup(cfg *config.KarmaxConfig) integration.ConfigLookup {
	return func(id string) map[string]string {
		out := map[string]string{}
		for _, ch := range cfg.Comms.Channels {
			if ch.ID != id && !strings.EqualFold(ch.Type, id) {
				continue
			}
			for k, v := range ch.Settings {
				out[k] = v
			}
			if ch.Token != "" {
				out["token"] = ch.Token
			}
		}
		return out
	}
}

// whatsApp is wacli's session, which KARMAX checks but does not own.
func whatsApp() integration.Integration {
	return integration.CLISession(integration.Manifest{
		ID:   "whatsapp",
		Name: "WhatsApp",
		Description: "Your own WhatsApp account, through wacli. KARMAX reads and sends as you, " +
			"so the session is a pairing on this machine rather than a token anyone can copy.",
		Kind:     integration.KindChannel,
		SetupURL: "https://github.com/MelloB1989/wacli",
	}, hostpaths.Wacli(), integration.CheckBinarySession(hostpaths.Wacli(), "status"))
}

// googleWorkspace is the gws CLI's OAuth session.
func googleWorkspace() integration.Integration {
	return integration.CLISession(integration.Manifest{
		ID:          "google_workspace",
		Name:        "Google Workspace",
		Description: "Calendar, Gmail, Drive, Chat and Tasks through the gws CLI.",
		Kind:        integration.KindConnector,
		SetupURL:    "https://github.com/MelloB1989/google-workspace-cli",
	}, hostpaths.GWS(), integration.CheckBinarySession(hostpaths.GWS(), "auth", "status"))
}

// discord is a bot token.
func discord(id string) integration.Integration {
	return integration.APIKey(integration.Manifest{
		ID:          id,
		Name:        "Discord",
		Description: "A Discord bot KARMAX talks through.",
		Kind:        integration.KindChannel,
		SetupURL:    "https://discord.com/developers/applications",
		Config: []connectorkit.ConfigField{
			{Key: "token", Description: "the bot token", Required: true, Secret: true},
		},
	}, "token", checkBearer("https://discord.com/api/v10/users/@me", "token", "Bot "))
}

// slack needs two tokens: an app-level one to open the socket and a bot one to
// post. Both are asked for, because being handed one and told "invalid auth" is
// the least helpful possible outcome.
func slack(id string) integration.Integration {
	return integration.APIKey(integration.Manifest{
		ID:          id,
		Name:        "Slack",
		Description: "A Slack app in Socket Mode — no public URL needed.",
		Kind:        integration.KindChannel,
		SetupURL:    "https://api.slack.com/apps",
		Config: []connectorkit.ConfigField{
			{Key: "bot_token", Description: "xoxb-… , from OAuth & Permissions", Required: true, Secret: true},
			{Key: "app_token", Description: "xapp-… , from Basic Information → App-Level Tokens", Required: true, Secret: true},
		},
	}, "bot_token", checkSlack)
}

// telegram is a bot token from BotFather.
func telegram(id string) integration.Integration {
	return integration.APIKey(integration.Manifest{
		ID:          id,
		Name:        "Telegram",
		Description: "A Telegram bot KARMAX talks through.",
		Kind:        integration.KindChannel,
		SetupURL:    "https://t.me/BotFather",
		Config: []connectorkit.ConfigField{
			{Key: "token", Description: "the token BotFather gave you", Required: true, Secret: true},
		},
	}, "token", checkTelegram)
}

// checkBearer verifies a token by calling an endpoint that needs one.
//
// A health check has to make a real call. "The config field is non-empty" is
// not health — it is exactly the state an expired token is also in.
func checkBearer(url, field, prefix string) func(context.Context, connectorkit.Credentials) error {
	return func(ctx context.Context, c connectorkit.Credentials) error {
		token := strings.TrimSpace(c.Get(field))
		if token == "" {
			return fmt.Errorf("no %s configured", field)
		}
		_, err := get(ctx, url, map[string]string{"Authorization": prefix + token})
		return err
	}
}

func checkTelegram(ctx context.Context, c connectorkit.Credentials) error {
	token := strings.TrimSpace(c.Get("token"))
	if token == "" {
		return fmt.Errorf("no bot token configured")
	}
	body, err := get(ctx, "https://api.telegram.org/bot"+token+"/getMe", nil)
	if err != nil {
		return err
	}
	// Telegram answers 200 with {"ok":false} rather than a status code, so the
	// body is the only thing that distinguishes a live token from a dead one.
	return expectJSONOK(body, "Telegram")
}

func checkSlack(ctx context.Context, c connectorkit.Credentials) error {
	token := strings.TrimSpace(c.Get("bot_token"))
	if token == "" {
		return fmt.Errorf("no bot token configured")
	}
	body, err := get(ctx, "https://slack.com/api/auth.test",
		map[string]string{"Authorization": "Bearer " + token})
	if err != nil {
		return err
	}
	// Same shape as Telegram: a revoked Slack app returns 200 with
	// {"ok":false,"error":"invalid_auth"}, so checking the status code alone
	// would report it as working.
	return expectJSONOK(body, "Slack")
}

// expectJSONOK reads the {"ok":bool,"error":string} envelope both Slack and
// Telegram use to report failure inside a 200.
func expectJSONOK(body []byte, who string) error {
	var res struct {
		OK          bool   `json:"ok"`
		Error       string `json:"error"`
		Description string `json:"description"`
	}
	if err := json.Unmarshal(body, &res); err != nil {
		return fmt.Errorf("%s returned something that is not JSON: %.150s", who, body)
	}
	if res.OK {
		return nil
	}
	reason := firstNonEmpty(res.Error, res.Description, "no reason given")
	return fmt.Errorf("%s refused the credentials: %s", who, reason)
}

// get performs one bounded health request.
func get(ctx context.Context, url string, headers map[string]string) ([]byte, error) {
	cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(cctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	switch {
	case resp.StatusCode == http.StatusUnauthorized, resp.StatusCode == http.StatusForbidden:
		return body, fmt.Errorf("the credentials were refused (%s)", resp.Status)
	case resp.StatusCode < 200 || resp.StatusCode > 299:
		return body, fmt.Errorf("%s answered %s", req.URL.Host, resp.Status)
	}
	return body, nil
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if s := strings.TrimSpace(v); s != "" {
			return s
		}
	}
	return ""
}
