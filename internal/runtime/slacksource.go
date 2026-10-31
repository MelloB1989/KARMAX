package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/MelloB1989/karmax/internal/bus"
	"github.com/MelloB1989/karmax/internal/integration"
	"github.com/MelloB1989/karmax/internal/integrations"
	"github.com/MelloB1989/karmax/internal/sources"
	slacksrc "github.com/MelloB1989/karmax/internal/sources/slack"
	"github.com/MelloB1989/karmax/internal/tools"
	"github.com/MelloB1989/karmax/internal/tools/builtin"
	"github.com/MelloB1989/karmax/pkg/loopkit"
	"go.uber.org/zap"
)

// slackSourceConfig reads the Slack source's settings, and reports false when
// KARMAX_SOURCE_SLACK=off.
//
//	KARMAX_SOURCE_SLACK_EVERY          how often the workspaces are polled (default 2m)
//	KARMAX_SOURCE_SLACK_LOOKBACK       how far back a conversation never read is read (default 2h)
//	KARMAX_SOURCE_SLACK_ACTIVE         a conversation this recently active is read every poll (default 24h)
//	KARMAX_SOURCE_SLACK_DORMANT        how often a quieter one is read (default 15m)
//	KARMAX_SOURCE_SLACK_CALLS          history reads per workspace per poll (default 40)
//	KARMAX_SOURCE_SLACK_MENTION_QUERY  search for mentions in threads; {user} is the operator's id; "off" disables
//	KARMAX_SOURCE_SLACK_DENY           conversations never read: ids or channel names, comma-separated
func slackSourceConfig(getenv func(string) string) (slacksrc.Config, string, bool) {
	get := func(k string) string { return strings.TrimSpace(getenv("KARMAX_SOURCE_SLACK" + k)) }
	if strings.EqualFold(get(""), "off") {
		return slacksrc.Config{}, "", false
	}
	cfg := slacksrc.Config{
		Lookback:     durationSetting(get("_LOOKBACK")),
		ActiveWindow: durationSetting(get("_ACTIVE")),
		DormantEvery: durationSetting(get("_DORMANT")),
		MentionQuery: get("_MENTION_QUERY"),
		Deny:         splitCSV(get("_DENY")),
	}
	cfg.CallsPerTick, _ = strconv.Atoi(get("_CALLS"))
	every := get("_EVERY")
	if durationSetting(every) <= 0 {
		every = "2m"
	}
	return cfg, every, true
}

// registerSlackSource registers the loop that turns the operator's Slack into
// events, and the tool that answers as them.
func (rt *KarmaxRuntime) registerSlackSource(reg *integration.Registry) {
	cfg, every, on := slackSourceConfig(os.Getenv)
	if !on {
		rt.log.Info("slack source is off (KARMAX_SOURCE_SLACK=off)")
		return
	}
	agentID := rt.primaryAgentID()
	src := slacksrc.New(cfg, rt.store, func(e sources.Event) error {
		return rt.bus.Publish(e.BusEvent(agentID))
	})
	workspaces := func() []slacksrc.Workspace { return integrations.SlackWorkspaces(rt.cfg, reg) }

	tool := &slackSendTool{
		send: func(ctx context.Context, ws, ch, thread, text string) (slacksrc.Sent, error) {
			return src.Send(ctx, workspaces(), ws, ch, thread, text)
		},
		// The operator lets KARMAX speak for them and is told afterwards.
		tell: func(title, body string) { builtin.PushAppNotification(rt.store, agentID, "update", title, body) },
	}
	if rt.tools != nil {
		rt.tools.Register(tool)
	}
	rt.lendToSource(slacksrc.Name, tool)

	var lastOff, lastNotes string
	loopkit.Register(loopkit.Loop{
		Name: "slack-source",
		Description: "Reads the operator's Slack in every connected workspace — DMs, mentions and the channels " +
			"they are in — and hands what is new to the agent as events; DMs and mentions are urgent.",
		Schedule: loopkit.Every(every),
		Run: func(ctx context.Context, k loopkit.Kit) error {
			res, err := src.Tick(ctx, workspaces())
			// Said when it changes, not every poll: an unconfigured source is
			// a state, not a failure.
			if res.Off != lastOff {
				if res.Off != "" {
					k.Logf("slack-source: %s", res.Off)
				} else {
					k.Logf("slack-source: on")
				}
				lastOff = res.Off
			}
			if res.Published > 0 {
				k.Logf("slack-source: %d workspaces, %d conversations read, %d events", res.Workspaces, res.Read, res.Published)
			}
			var notes []string
			for _, st := range src.Status() {
				if st.Note != "" {
					notes = append(notes, st.Account+": "+st.Note)
				}
			}
			if n := strings.Join(notes, "; "); n != lastNotes {
				if n != "" {
					k.Logf("slack-source: %s", n)
				}
				lastNotes = n
			}
			return err
		},
	})
	rt.log.Info("slack source registered", zap.String("every", every))
}

// primaryAgentID is the agent source events are delivered to.
func (rt *KarmaxRuntime) primaryAgentID() string {
	if rt.cfg != nil && len(rt.cfg.Agents) > 0 {
		return rt.cfg.Agents[0].ID
	}
	return ""
}

// lendToSource lends a tool to every turn that handles one of a source's
// events, so the agent can act on an event without the tool being in its
// standing allowlist.
func (rt *KarmaxRuntime) lendToSource(source string, t tools.Tool) {
	rt.sourceToolsMu.Lock()
	defer rt.sourceToolsMu.Unlock()
	if rt.sourceTools == nil {
		rt.sourceTools = map[string][]tools.Tool{}
	}
	rt.sourceTools[source] = append(rt.sourceTools[source], t)
}

// sourceToolsForEvent is what a source event's turn is lent.
func (rt *KarmaxRuntime) sourceToolsForEvent(evt bus.Event) []tools.Tool {
	if evt.Kind != bus.EventWebhookFired || evt.Payload == nil {
		return nil
	}
	src, _ := evt.Payload["source"].(string)
	if src == "" {
		return nil
	}
	rt.sourceToolsMu.Lock()
	defer rt.sourceToolsMu.Unlock()
	return append([]tools.Tool(nil), rt.sourceTools[src]...)
}

// slackSendTool posts as the operator.
type slackSendTool struct {
	send func(ctx context.Context, workspace, channel, threadTS, text string) (slacksrc.Sent, error)
	tell func(title, body string)
}

func (t *slackSendTool) Manifest() tools.ToolManifest {
	return tools.ToolManifest{
		Name: "slack.send_as_operator",
		Description: "Send a Slack message AS THE OPERATOR — from their own account, not a bot — to answer a DM, " +
			"a mention or a thread. Take workspace, channel and thread_ts from the Slack event's data; an empty " +
			"thread_ts posts in the DM or channel itself. The operator allows this freely and is told afterwards " +
			"what was sent, so write exactly what they would.",
		Parameters: json.RawMessage(`{
			"type": "object",
			"properties": {
				"workspace": {"type": "string", "description": "The event's workspace (its account); may be omitted when only one is connected."},
				"channel": {"type": "string", "description": "Conversation id from the event: D… for a DM, C…/G… for a channel."},
				"thread_ts": {"type": "string", "description": "Reply inside this thread (from the event's thread_ts)."},
				"text": {"type": "string", "description": "The message, in Slack mrkdwn."}
			},
			"required": ["channel", "text"]
		}`),
	}
}

func (t *slackSendTool) Execute(ctx context.Context, in map[string]any) (tools.ToolResult, error) {
	arg := func(k string) string { v, _ := in[k].(string); return strings.TrimSpace(v) }
	text, channel := arg("text"), arg("channel")
	if text == "" || channel == "" {
		return tools.ErrorResult(fmt.Errorf("channel and text are required")), nil
	}
	if t.send == nil {
		return tools.ErrorResult(fmt.Errorf("the Slack source is not running")), nil
	}
	sent, err := t.send(ctx, arg("workspace"), channel, arg("thread_ts"), text)
	if err != nil {
		return tools.ErrorResult(err), nil
	}
	if t.tell != nil {
		body := fmt.Sprintf("In %s (%s): %s", sent.Workspace, sent.Channel, clip(text, 240))
		if sent.URL != "" {
			body += "\n" + sent.URL
		}
		t.tell("Sent as you on Slack", body)
	}
	return tools.SuccessResult(map[string]any{"posted": true, "workspace": sent.Workspace, "channel": sent.Channel,
		"ts": sent.TS, "url": sent.URL}), nil
}

func durationSetting(s string) time.Duration {
	d, err := time.ParseDuration(strings.TrimSpace(s))
	if err != nil || d <= 0 {
		return 0
	}
	return d
}
