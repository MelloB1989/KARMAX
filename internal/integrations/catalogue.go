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
	"os"
	"sort"
	"strings"
	"time"

	"github.com/MelloB1989/karmax/internal/config"
	githubconn "github.com/MelloB1989/karmax/internal/connectors/github"
	instagramconn "github.com/MelloB1989/karmax/internal/connectors/instagram"
	notionconn "github.com/MelloB1989/karmax/internal/connectors/notion"
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

	// The connectors, adapted through their own manifests — so a connector and a
	// channel are the same kind of thing to `karmax login`, which is the point
	// of having one registry at all.
	reg.Register(integration.FromConnector(githubconn.New(""), ""))
	for _, account := range splitCSV(os.Getenv("KARMAX_GITHUB_ACCOUNTS")) {
		reg.Register(integration.FromConnector(githubconn.New(account), account))
	}
	reg.Register(integration.FromConnector(notionconn.New(), ""))
	reg.Register(integration.FromConnector(instagramconn.New(), ""))
	// The public accounts, present for authentication ONLY. They carry no tools:
	// publishing left the registry entirely and is reachable from a loop through
	// social_authorize, so the orchestrator cannot hold it. What stays here is
	// `karmax login linkedin` and a health check, which is what keeps a token
	// refreshed for the loop that does the posting.
	reg.Register(socialAccount("x", "X", "Post to X as you.",
		"https://developer.x.com/en/portal/dashboard", checkX))
	reg.Register(socialAccount("linkedin", "LinkedIn", "Post to LinkedIn as you.",
		"https://www.linkedin.com/developers/apps", checkLinkedIn))

	// The token-based channels, one per configured channel so two Slack
	// workspaces are two integrations rather than one that silently wins.
	for _, ch := range cfg.Comms.Channels {
		if build, ok := channelIntegrations[strings.ToLower(ch.Type)]; ok {
			reg.Register(build(ch.ID))
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

// splitCSV reads a comma-separated environment list.
func splitCSV(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if s := strings.TrimSpace(v); s != "" {
			return s
		}
	}
	return ""
}

// channelIntegrations are the channel types that become an integration once a
// channel of that type is configured — one per channel, so two Slack
// workspaces are two integrations rather than one that silently wins.
//
// A map rather than a switch because the CLI needs the same list to answer
// "what could I connect that I have not", and a second copy written by hand is
// a copy that drifts.
var channelIntegrations = map[string]func(id string) integration.Integration{
	"discord":  discord,
	"slack":    slack,
	"telegram": telegram,
}

// SupportedChannelTypes names every channel type KARMAX can run, sorted.
//
// Registration is per configured channel, which means a supported channel type
// nobody has configured is absent from `karmax integrations` and therefore
// indistinguishable from one KARMAX cannot do at all. Discord and Telegram have
// been implemented and wired the whole time; the operator had no way to find out.
func SupportedChannelTypes() []string {
	out := make([]string, 0, len(channelIntegrations))
	for name := range channelIntegrations {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// UnconfiguredChannelTypes names supported channel types with no channel
// configured for them.
func UnconfiguredChannelTypes(cfg *config.KarmaxConfig) []string {
	configured := map[string]bool{}
	if cfg != nil {
		for _, ch := range cfg.Comms.Channels {
			configured[strings.ToLower(ch.Type)] = true
		}
	}
	var out []string
	for _, name := range SupportedChannelTypes() {
		if !configured[name] {
			out = append(out, name)
		}
	}
	return out
}

// socialAccount is an account KARMAX authenticates but does not act through.
//
// The connector that used to own these also owned linkedin.post and x.post,
// which put publishing in the tool registry where the orchestrator could hold
// it. Only the auth half remains: the loop that publishes asks the host for a
// credential at the moment it posts, and gets one only for text the privacy
// guard has already cleared.
func socialAccount(id, name, desc, setupURL string,
	check func(context.Context, connectorkit.Credentials) error) integration.Integration {

	return integration.Simple{
		Meta: integration.Manifest{
			ID: id, Name: name, Description: desc,
			Kind: integration.KindConnector, SetupURL: setupURL,
			Config: []connectorkit.ConfigField{
				{Key: "client_id", Description: "From your app's auth page", Required: true},
				{Key: "client_secret", Description: "From the same page", Required: true, Secret: true},
			},
		},
		Method: connectorkit.AuthMethod{
			Kind:   connectorkit.AuthOAuth2,
			OAuth2: oauthFor(id),
		},
		CheckFunc: check,
	}
}

// oauthFor is each platform's browser flow.
func oauthFor(id string) *connectorkit.OAuth2Config {
	switch id {
	case "linkedin":
		return &connectorkit.OAuth2Config{
			AuthURL:     "https://www.linkedin.com/oauth/v2/authorization",
			TokenURL:    "https://www.linkedin.com/oauth/v2/accessToken",
			Scopes:      []string{"openid", "profile", "w_member_social"},
			ClientIDKey: "client_id", SecretKey: "client_secret",
		}
	case "x":
		return &connectorkit.OAuth2Config{
			AuthURL:     "https://x.com/i/oauth2/authorize",
			TokenURL:    "https://api.x.com/2/oauth2/token",
			Scopes:      []string{"tweet.read", "tweet.write", "users.read", "offline.access"},
			ClientIDKey: "client_id", SecretKey: "client_secret",
		}
	}
	return nil
}

// checkLinkedIn and checkX verify a stored token by calling the platform.
func checkLinkedIn(ctx context.Context, c connectorkit.Credentials) error {
	if strings.TrimSpace(c.AccessToken) == "" {
		return fmt.Errorf("linkedin: not signed in — run `karmax login linkedin`")
	}
	_, err := get(ctx, "https://api.linkedin.com/v2/userinfo",
		map[string]string{"Authorization": "Bearer " + c.AccessToken})
	return err
}

func checkX(ctx context.Context, c connectorkit.Credentials) error {
	if strings.TrimSpace(c.AccessToken) == "" {
		return fmt.Errorf("x: not signed in — run `karmax login x`")
	}
	_, err := get(ctx, "https://api.x.com/2/users/me",
		map[string]string{"Authorization": "Bearer " + c.AccessToken})
	return err
}
