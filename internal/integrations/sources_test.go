package integrations

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/MelloB1989/karmax/internal/config"
	"github.com/MelloB1989/karmax/internal/integration"
	"github.com/MelloB1989/karmax/pkg/connectorkit"
)

func field(m integration.Manifest, key string) (connectorkit.ConfigField, bool) {
	for _, f := range m.Config {
		if f.Key == key {
			return f, true
		}
	}
	return connectorkit.ConfigField{}, false
}

// `karmax login posthog` stores a personal API key the way every other
// connector's key is stored, and a second login is an account.
func TestPostHogIsAnIntegrationLoginCanStore(t *testing.T) {
	t.Setenv("KARMAX_POSTHOG_ACCOUNTS", "work")
	reg := Build(&config.KarmaxConfig{}, nil)

	for _, id := range []string{"posthog", "posthog:work"} {
		in, ok := reg.Get(id)
		if !ok {
			t.Fatalf("%s is not registered", id)
		}
		m := in.Manifest()
		if in.Auth().Kind != connectorkit.AuthAPIKey || in.Auth().APIKeyField != "posthog_api_key" {
			t.Errorf("%s auth %+v", id, in.Auth())
		}
		key, ok := field(m, "posthog_api_key")
		if !ok || !key.Secret || !key.Required {
			t.Errorf("%s: the key must be asked for, required and never echoed: %+v", id, key)
		}
		if _, ok := field(m, "posthog_host"); !ok {
			t.Errorf("%s does not ask for the host", id)
		}
		if _, ok := field(m, "posthog_project_ids"); !ok {
			t.Errorf("%s does not ask for project ids", id)
		}
	}
	// Unconfigured, its health says exactly what to run.
	err := mustIntegration(t, reg, "posthog").Health(context.Background(), connectorkit.Credentials{})
	if err == nil || !strings.Contains(err.Error(), "karmax login posthog") {
		t.Errorf("unconfigured health: %v", err)
	}
}

func mustIntegration(t *testing.T, reg *integration.Registry, id string) integration.Integration {
	t.Helper()
	in, ok := reg.Get(id)
	if !ok {
		t.Fatalf("%s is not registered", id)
	}
	return in
}

// The environment names a key can come from are PostHog's own, never a bare
// API_KEY some other tool set.
func TestPostHogAccountsReadTheirOwnEnvironment(t *testing.T) {
	t.Setenv("API_KEY", "not-for-posthog")
	t.Setenv("POSTHOG_API_KEY", "phx_from_env")
	t.Setenv("POSTHOG_PROJECT_IDS", "101, 202")
	reg := Build(&config.KarmaxConfig{}, nil)

	accs := PostHogAccounts(reg)
	if len(accs) != 1 || accs[0].Integration != "posthog" || accs[0].APIKey != "phx_from_env" {
		t.Fatalf("accounts %+v", redactAccounts(accs))
	}
	if strings.Join(accs[0].ProjectIDs, ",") != "101,202" {
		t.Errorf("project ids %v", accs[0].ProjectIDs)
	}
}

func redactAccounts[T any](v []T) int { return len(v) }

// The Slack login asks for the operator's own user token, which is what lets
// KARMAX read their DMs and mentions and answer as them.
func TestSlackLoginAsksForTheOperatorsUserToken(t *testing.T) {
	cfg := &config.KarmaxConfig{Comms: config.CommsConfig{Channels: []config.ChannelConfig{{ID: "slack-main", Type: "slack"}}}}
	f, ok := field(mustIntegration(t, Build(cfg, nil), "slack-main").Manifest(), "user_token")
	if !ok || !f.Secret || !f.Required || !strings.Contains(f.Description, "xoxp-") {
		t.Errorf("user_token field %+v", f)
	}
	// Turned off, Slack events do not make anybody paste a token they do not need.
	t.Setenv("KARMAX_SOURCE_SLACK", "off")
	f, _ = field(mustIntegration(t, Build(cfg, nil), "slack-main").Manifest(), "user_token")
	if f.Required {
		t.Error("user_token is required although Slack events are off")
	}
}

// The channel's health says when the bot works but events cannot be read,
// and refuses a bot token pasted as the user token.
func TestSlackHealthCoversTheUserToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Header.Get("Authorization") {
		case "Bearer xoxb-good", "Bearer xoxp-good":
			_, _ = w.Write([]byte(`{"ok":true,"team":"Northwind Labs","user":"op"}`))
		default:
			_, _ = w.Write([]byte(`{"ok":false,"error":"invalid_auth"}`))
		}
	}))
	defer srv.Close()
	old := slackAPI
	slackAPI = srv.URL
	defer func() { slackAPI = old }()

	creds := func(kv map[string]string) connectorkit.Credentials { return connectorkit.Credentials{Config: kv} }
	ctx := context.Background()

	err := checkSlackFor("slack-main")(ctx, creds(map[string]string{"bot_token": "xoxb-good"}))
	if err == nil || !strings.Contains(err.Error(), "karmax login slack-main") || !strings.Contains(err.Error(), "xoxp-") {
		t.Errorf("bot without user token: %v", err)
	}
	if err := checkSlackFor("slack-main")(ctx, creds(map[string]string{"bot_token": "xoxb-good", "user_token": "xoxp-good"})); err != nil {
		t.Errorf("both tokens good: %v", err)
	}
	err = checkSlackFor("slack-main")(ctx, creds(map[string]string{"bot_token": "xoxb-good", "user_token": "xoxb-good"}))
	if err == nil || !strings.Contains(err.Error(), "xoxp-") {
		t.Errorf("a bot token as the user token: %v", err)
	}
	err = checkSlackFor("slack-main")(ctx, creds(map[string]string{"bot_token": "xoxb-good", "user_token": "xoxp-revoked"}))
	if err == nil || !strings.Contains(err.Error(), "user token") {
		t.Errorf("a refused user token: %v", err)
	}
	t.Setenv("KARMAX_SOURCE_SLACK", "off")
	if err := checkSlackFor("slack-main")(ctx, creds(map[string]string{"bot_token": "xoxb-good"})); err != nil {
		t.Errorf("with Slack events off, a bot alone is healthy: %v", err)
	}
}

// Every Slack channel is a workspace the source reads, with its tokens.
func TestSlackWorkspacesAreTheConfiguredChannels(t *testing.T) {
	t.Setenv("SLACK_USER_TOKEN", "xoxp-env")
	cfg := &config.KarmaxConfig{Comms: config.CommsConfig{Channels: []config.ChannelConfig{
		{ID: "slack-main", Type: "slack"},
		{ID: "slack-contoso", Type: "Slack", Settings: map[string]string{"user_token": "xoxp-contoso", "bot_token": "xoxb-contoso"}},
		{ID: "wa", Type: "whatsapp"},
	}}}
	wss := SlackWorkspaces(cfg, Build(cfg, nil))
	if len(wss) != 2 {
		t.Fatalf("%d workspaces", len(wss))
	}
	if wss[0].Integration != "slack-main" || wss[0].UserToken != "xoxp-env" {
		t.Errorf("the first channel takes SLACK_USER_TOKEN: %s", wss[0].Integration)
	}
	if wss[1].Integration != "slack-contoso" || wss[1].UserToken != "xoxp-contoso" || wss[1].BotToken != "xoxb-contoso" {
		t.Errorf("the second channel's own settings: %s", wss[1].Integration)
	}
}
