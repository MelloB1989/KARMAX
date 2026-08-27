package slack

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/MelloB1989/karmax/pkg/connectorkit"
)

func creds(kv map[string]string) connectorkit.Credentials {
	return connectorkit.Credentials{Config: kv}
}

func stub(t *testing.T, body string) func() {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(body))
	}))
	old := api
	api = srv.URL
	return func() { api = old; srv.Close() }
}

// Slack answers errors with HTTP 200 and {"ok":false}. Trusting the status code
// means every failure looks like a success with strange contents.
func TestAnErrorArrivesAsHTTP200(t *testing.T) {
	defer stub(t, `{"ok":false,"error":"invalid_auth"}`)()

	err := call(context.Background(), "xoxb-x", "auth.test", nil, nil)
	if err == nil {
		t.Fatal("an ok:false response was treated as success")
	}
	if !strings.Contains(err.Error(), "rejected") {
		t.Errorf("unhelpful message: %v", err)
	}
}

// The errors an operator actually hits should say what to do about them.
func TestErrorsSayWhatToDo(t *testing.T) {
	for code, want := range map[string]string{
		"invalid_auth":      "regenerate it",
		"missing_scope":     "REINSTALL",
		"not_in_channel":    "/invite",
		"channel_not_found": "channel id",
	} {
		err := slackError("chat.postMessage", code, "chat:write")
		if !strings.Contains(err.Error(), want) {
			t.Errorf("%s -> %q, expected it to mention %q", code, err, want)
		}
	}
}

// A bot token that works while the app token is missing looks healthy and
// cannot hear anything — the exact failure this install spent a day on.
func TestHealthFlagsAMissingAppToken(t *testing.T) {
	defer stub(t, `{"ok":true,"user":"lamb_ocrew","team":"Zero Moblt"}`)()
	t.Setenv("SLACK_APP_TOKEN", "")

	err := New().Health(context.Background(), creds(map[string]string{"bot_token": "xoxb-x"}))
	if err == nil {
		t.Fatal("health passed with no app-level token — the bot could post but never receive")
	}
	if !strings.Contains(err.Error(), "xapp-") || !strings.Contains(err.Error(), "connections:write") {
		t.Errorf("the message does not say how to fix it: %v", err)
	}

	// With both tokens it is genuinely healthy.
	if err := New().Health(context.Background(), creds(map[string]string{
		"bot_token": "xoxb-x", "app_token": "xapp-x",
	})); err != nil {
		t.Errorf("health failed with both tokens: %v", err)
	}
}

// This install's token lives in the daemon's .env. Reporting "not configured"
// next to a bot that is visibly answering would be false.
func TestTheEnvironmentTokenIsARecognisedFallback(t *testing.T) {
	t.Setenv("SLACK_BOT_TOKEN", "xoxb-from-env")

	if got := botToken(creds(map[string]string{})); got != "xoxb-from-env" {
		t.Errorf("env token not picked up, got %q", got)
	}
	// A configured credential wins, so configuring it properly later migrates.
	if got := botToken(creds(map[string]string{"bot_token": "xoxb-saved"})); got != "xoxb-saved" {
		t.Errorf("the saved credential should win, got %q", got)
	}
}

func TestChannelsReportWhetherTheBotCanPost(t *testing.T) {
	defer stub(t, `{"ok":true,"channels":[{"id":"C1","name":"eng","is_member":false}]}`)()

	res, err := channels(context.Background(), creds(map[string]string{"bot_token": "x"}), nil)
	if err != nil {
		t.Fatal(err)
	}
	list := res.(map[string]any)["channels"].([]map[string]any)
	// Posting to a channel the bot is not in fails with not_in_channel, so the
	// model should be able to see that before trying.
	if list[0]["bot_is_member"] != false {
		t.Errorf("membership not reported: %#v", list[0])
	}
}
