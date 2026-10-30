package integrations

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/MelloB1989/karmax/internal/config"
	"github.com/MelloB1989/karmax/internal/integration"
	posthogsrc "github.com/MelloB1989/karmax/internal/sources/posthog"
	slacksrc "github.com/MelloB1989/karmax/internal/sources/slack"
	"github.com/MelloB1989/karmax/pkg/connectorkit"
)

// The event sources' credentials, resolved the same way as every other
// integration's: `karmax login` first, then karmax.yaml, then the environment.
// The daemon calls these on every poll, so connecting an account turns its
// source on without a restart.

// SourceOn reports whether an event source is on: KARMAX_SOURCE_<NAME>=off
// turns it off.
func SourceOn(name string) bool {
	v := os.Getenv("KARMAX_SOURCE_" + strings.ToUpper(name))
	return !strings.EqualFold(strings.TrimSpace(v), "off")
}

// SlackWorkspaces is every configured Slack channel as a workspace the Slack
// source reads: the operator's user token, and the bot's, which only serves
// to recognise KARMAX's own messages. SLACK_USER_TOKEN and SLACK_BOT_TOKEN
// stand in for the first channel, as SLACK_BOT_TOKEN always has.
func SlackWorkspaces(cfg *config.KarmaxConfig, reg *integration.Registry) []slacksrc.Workspace {
	var out []slacksrc.Workspace
	for _, ch := range cfg.Comms.Channels {
		if !strings.EqualFold(ch.Type, "slack") {
			continue
		}
		creds, _, _ := reg.Credentials(ch.ID)
		w := slacksrc.Workspace{
			Integration: ch.ID,
			UserToken:   strings.TrimSpace(creds.Get("user_token")),
			BotToken:    firstNonEmpty(creds.Get("bot_token"), ch.Token),
		}
		if len(out) == 0 {
			w.UserToken = firstNonEmpty(w.UserToken, os.Getenv("SLACK_USER_TOKEN"))
			w.BotToken = firstNonEmpty(w.BotToken, os.Getenv("SLACK_BOT_TOKEN"))
		}
		out = append(out, w)
	}
	if len(out) == 0 {
		if u := strings.TrimSpace(os.Getenv("SLACK_USER_TOKEN")); u != "" {
			out = append(out, slacksrc.Workspace{UserToken: u, BotToken: strings.TrimSpace(os.Getenv("SLACK_BOT_TOKEN"))})
		}
	}
	return out
}

// postHogIDs are the PostHog integrations: the primary, and one per name in
// KARMAX_POSTHOG_ACCOUNTS for keys to other organisations.
func postHogIDs() []string {
	ids := []string{"posthog"}
	for _, a := range splitCSV(os.Getenv("KARMAX_POSTHOG_ACCOUNTS")) {
		ids = append(ids, "posthog:"+a)
	}
	return ids
}

// PostHogAccounts is every PostHog login, keyed or not; the source reports
// the ones without a key as off.
func PostHogAccounts(reg *integration.Registry) []posthogsrc.Account {
	var out []posthogsrc.Account
	for _, id := range postHogIDs() {
		creds, _, _ := reg.Credentials(id)
		out = append(out, postHogAccount(id, creds))
	}
	return out
}

func postHogAccount(id string, c connectorkit.Credentials) posthogsrc.Account {
	return posthogsrc.Account{
		Integration: id,
		APIKey:      strings.TrimSpace(c.Get("posthog_api_key")),
		Host:        c.Get("posthog_host"),
		ProjectIDs:  splitCSV(c.Get("posthog_project_ids")),
	}
}

// postHog is a personal API key. The field names carry the provider so the
// bare environment fallback reads POSTHOG_API_KEY rather than whatever API_KEY
// some other tool left in the daemon's environment.
func postHog(id string) integration.Integration {
	_, account := integration.SplitID(id)
	return integration.APIKey(integration.Manifest{
		ID:   id,
		Name: "PostHog",
		Description: "Product analytics as events: new and spiking errors, insight alerts going off, " +
			"and a daily look at the busiest metrics.",
		Kind:     integration.KindConnector,
		SetupURL: "https://us.posthog.com/settings/user-api-keys",
		Account:  account,
		Config: []connectorkit.ConfigField{
			{Key: "posthog_api_key", Description: "personal API key (phx_…) with the scopes " + posthogsrc.Scopes +
				", from Settings → Personal API keys", Required: true, Secret: true},
			{Key: "posthog_host", Description: "https://us.posthog.com, https://eu.posthog.com, or your own instance",
				Default: "https://us.posthog.com"},
			{Key: "posthog_project_ids", Description: "project ids to watch, comma-separated; empty watches every project the key can see"},
		},
	}, "posthog_api_key", func(ctx context.Context, c connectorkit.Credentials) error {
		a := postHogAccount(id, c)
		if a.APIKey == "" && !SourceOn(posthogsrc.Name) {
			return nil
		}
		return posthogsrc.Health(ctx, a)
	})
}

// slackAPI is the Web API base the channel's health check calls.
var slackAPI = "https://slack.com/api"

// checkSlackFor verifies a Slack channel's tokens: the bot's, and the
// operator's user token the Slack source reads and answers with.
func checkSlackFor(id string) func(context.Context, connectorkit.Credentials) error {
	return func(ctx context.Context, c connectorkit.Credentials) error {
		token := strings.TrimSpace(c.Get("bot_token"))
		if token == "" {
			need := "bot_token (xoxb-) and app_token (xapp-)"
			if SourceOn(slacksrc.Name) {
				need += ", and your user_token (xoxp-) with the scopes " + slacksrc.Scopes
			}
			return fmt.Errorf("no bot token configured — create or open the app at https://api.slack.com/apps, "+
				"install it to the workspace, then run `karmax login %s` and paste its %s", id, need)
		}
		if err := slackAuthTest(ctx, token); err != nil {
			return err
		}
		user := strings.TrimSpace(c.Get("user_token"))
		switch {
		case user == "" && SourceOn(slacksrc.Name):
			return fmt.Errorf("the bot works; %s", slacksrc.OffReason(id, "no user token (xoxp-…) for "+id))
		case user == "":
			return nil
		case !strings.Contains(user, "xoxp-"):
			return fmt.Errorf("user_token should be your User OAuth Token (xoxp-…), not a bot or app token; " +
				"copy it from OAuth & Permissions")
		}
		if err := slackAuthTest(ctx, user); err != nil {
			return fmt.Errorf("the user token: %w", err)
		}
		return nil
	}
}

func slackAuthTest(ctx context.Context, token string) error {
	body, err := get(ctx, slackAPI+"/auth.test", map[string]string{"Authorization": "Bearer " + token})
	if err != nil {
		return err
	}
	// A revoked Slack app returns 200 with {"ok":false,"error":"invalid_auth"},
	// so checking the status code alone would report it as working.
	return expectJSONOK(body, "Slack")
}
