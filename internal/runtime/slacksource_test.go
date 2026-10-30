package runtime

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/MelloB1989/karmax/internal/bus"
	"github.com/MelloB1989/karmax/internal/sources"
	slacksrc "github.com/MelloB1989/karmax/internal/sources/slack"
	"github.com/MelloB1989/karmax/internal/tools"
)

// Every knob of the Slack source is configuration, and one switch turns it off.
func TestSlackSourceSettingsComeFromTheEnvironment(t *testing.T) {
	env := map[string]string{
		"KARMAX_SOURCE_SLACK_EVERY":         "90s",
		"KARMAX_SOURCE_SLACK_LOOKBACK":      "6h",
		"KARMAX_SOURCE_SLACK_ACTIVE":        "48h",
		"KARMAX_SOURCE_SLACK_DORMANT":       "20m",
		"KARMAX_SOURCE_SLACK_CALLS":         "25",
		"KARMAX_SOURCE_SLACK_MENTION_QUERY": "off",
		"KARMAX_SOURCE_SLACK_DENY":          "#random, C0NOISE",
	}
	cfg, every, on := slackSourceConfig(func(k string) string { return env[k] })
	if !on || every != "90s" {
		t.Fatalf("on=%v every=%q", on, every)
	}
	if cfg.Lookback != 6*time.Hour || cfg.ActiveWindow != 48*time.Hour || cfg.DormantEvery != 20*time.Minute ||
		cfg.CallsPerTick != 25 || cfg.MentionQuery != "off" || strings.Join(cfg.Deny, ",") != "#random,C0NOISE" {
		t.Errorf("config %+v", cfg)
	}

	_, every, on = slackSourceConfig(func(string) string { return "" })
	if !on || every != "2m" {
		t.Errorf("defaults: on=%v every=%q", on, every)
	}
	env["KARMAX_SOURCE_SLACK"] = "OFF"
	if _, _, on := slackSourceConfig(func(k string) string { return env[k] }); on {
		t.Error("KARMAX_SOURCE_SLACK=off left the source on")
	}
}

// The turn handling a Slack event can answer as the operator; a turn handling
// anything else is not handed that power.
func TestASlackEventLendsTheSendAsOperatorTool(t *testing.T) {
	tool := &slackSendTool{}
	rt := &KarmaxRuntime{}
	rt.lendToSource(slacksrc.Name, tool)

	slackEvt := sources.Event{Source: "slack", Account: "northwind-labs", Key: "k"}.BusEvent("nexus")
	if !hasTool(rt.lentToolsForEvent(slackEvt), "slack.send_as_operator") {
		t.Error("a Slack event's turn cannot answer as the operator")
	}
	other := sources.Event{Source: "posthog", Account: "Storefront", Key: "k"}.BusEvent("nexus")
	if hasTool(rt.lentToolsForEvent(other), "slack.send_as_operator") {
		t.Error("a PostHog event's turn was handed the Slack send")
	}
	if hasTool(rt.lentToolsForEvent(bus.NewEvent(bus.EventCommsMessage, "nexus", map[string]any{"source": "slack"})), "slack.send_as_operator") {
		t.Error("only a source event lends a source's tools")
	}
}

func hasTool(ts []tools.Tool, name string) bool {
	for _, t := range ts {
		if t.Manifest().Name == name {
			return true
		}
	}
	return false
}

// Act freely, tell after: a message sent as the operator is reported to them.
func TestSendingAsTheOperatorTellsThemAfterwards(t *testing.T) {
	var told []string
	var sentTo []string
	tool := &slackSendTool{
		send: func(_ context.Context, ws, ch, thread, text string) (slacksrc.Sent, error) {
			sentTo = append(sentTo, ws+"|"+ch+"|"+thread+"|"+text)
			return slacksrc.Sent{Workspace: "northwind-labs", Channel: ch, TS: "1.2", URL: "https://example.test/p12"}, nil
		},
		tell: func(title, body string) { told = append(told, title+"\n"+body) },
	}
	res, err := tool.Execute(context.Background(), map[string]any{"workspace": "northwind-labs", "channel": "D0ALICE",
		"thread_ts": "", "text": "Reviewed, looks good."})
	if err != nil || res.IsError {
		t.Fatalf("execute: %v %+v", err, res)
	}
	if len(sentTo) != 1 || sentTo[0] != "northwind-labs|D0ALICE||Reviewed, looks good." {
		t.Errorf("sent %v", sentTo)
	}
	if len(told) != 1 || !strings.Contains(told[0], "Reviewed, looks good.") || !strings.Contains(told[0], "northwind-labs") {
		t.Errorf("the operator was told %v", told)
	}

	tool.send = func(context.Context, string, string, string, string) (slacksrc.Sent, error) {
		return slacksrc.Sent{}, errors.New("slack chat.postMessage: not_in_channel")
	}
	res, _ = tool.Execute(context.Background(), map[string]any{"channel": "C0ENG", "text": "x"})
	if !res.IsError || len(told) != 1 {
		t.Errorf("a failed send was reported as sent: %+v, told %d", res, len(told))
	}
	if res, _ := tool.Execute(context.Background(), map[string]any{"channel": "C0ENG"}); !res.IsError {
		t.Error("a send with no text was attempted")
	}
}
