package slack

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"

	"github.com/MelloB1989/karmax/pkg/connectorkit"
)

// Deliberately four tools, not a mirror of the Web API.
//
// Replying in a conversation is the comms channel's job and is not here: an
// agent that can reply through two different paths will eventually reply twice.
// What is here is what the channel cannot do — reach a channel nobody spoke in,
// and answer "who is that?" from a Slack id.

func (c *Connector) Tools() []connectorkit.Tool {
	return []connectorkit.Tool{
		{
			Name: "slack.post",
			Description: "Post a message to a Slack channel the agent was NOT spoken to in. " +
				"To reply where you were addressed, answer normally instead — that goes back to " +
				"the right thread on its own.",
			Parameters: json.RawMessage(`{
				"type":"object",
				"properties":{
					"channel":{"type":"string","description":"Channel id (C…) or #name. Falls back to the configured default."},
					"text":{"type":"string","description":"Message body. Slack mrkdwn."},
					"thread_ts":{"type":"string","description":"Reply inside this thread, if continuing one."}
				},
				"required":["text"]
			}`),
			Call: post,
		},
		{
			Name:        "slack.channels",
			Description: "List Slack channels the bot can see. Use to turn a channel name into an id.",
			Parameters: json.RawMessage(`{
				"type":"object",
				"properties":{"limit":{"type":"integer","description":"default 100, max 400"}}
			}`),
			Call: channels,
		},
		{
			Name:        "slack.user",
			Description: "Look up a Slack user by id, returning their real name and email where visible.",
			Parameters: json.RawMessage(`{
				"type":"object",
				"properties":{"user":{"type":"string","description":"Slack user id, e.g. U0123456789."}},
				"required":["user"]
			}`),
			Call: user,
		},
		{
			Name: "slack.history",
			Description: "Read recent messages from a Slack channel, oldest last. Use to catch up on " +
				"what was said somewhere before answering about it.",
			Parameters: json.RawMessage(`{
				"type":"object",
				"properties":{
					"channel":{"type":"string","description":"Channel id (C…)."},
					"limit":{"type":"integer","description":"default 20, max 100"}
				},
				"required":["channel"]
			}`),
			Call: history,
		},
	}
}

func post(ctx context.Context, cr connectorkit.Credentials, in map[string]any) (any, error) {
	text := strArg(in, "text")
	if strings.TrimSpace(text) == "" {
		return nil, fmt.Errorf("slack: text is required")
	}
	channel := strArg(in, "channel")
	if channel == "" {
		channel = strings.TrimSpace(cr.Get("default_channel"))
	}
	if channel == "" {
		return nil, fmt.Errorf("slack: no channel given and no default_channel configured — " +
			"call slack.channels to find one")
	}

	form := url.Values{"channel": {channel}, "text": {text}}
	if ts := strArg(in, "thread_ts"); ts != "" {
		form.Set("thread_ts", ts)
	}

	var res struct {
		Channel string `json:"channel"`
		TS      string `json:"ts"`
	}
	if err := call(ctx, botToken(cr), "chat.postMessage", form, &res); err != nil {
		return nil, err
	}
	return map[string]any{"posted": true, "channel": res.Channel, "ts": res.TS}, nil
}

func channels(ctx context.Context, cr connectorkit.Credentials, in map[string]any) (any, error) {
	limit := intArg(in, "limit", 100)
	if limit > 400 {
		limit = 400
	}
	form := url.Values{
		"limit": {fmt.Sprint(limit)},
		"types": {"public_channel,private_channel"},
		// Archived channels are noise in a list meant for choosing where to post.
		"exclude_archived": {"true"},
	}

	var res struct {
		Channels []struct {
			ID       string `json:"id"`
			Name     string `json:"name"`
			IsMember bool   `json:"is_member"`
		} `json:"channels"`
	}
	if err := call(ctx, botToken(cr), "conversations.list", form, &res); err != nil {
		return nil, err
	}

	out := make([]map[string]any, 0, len(res.Channels))
	for _, ch := range res.Channels {
		// is_member matters: posting to a channel the bot is not in fails with
		// not_in_channel, so the model should be able to see that in advance.
		out = append(out, map[string]any{"id": ch.ID, "name": ch.Name, "bot_is_member": ch.IsMember})
	}
	return map[string]any{"count": len(out), "channels": out}, nil
}

func user(ctx context.Context, cr connectorkit.Credentials, in map[string]any) (any, error) {
	id := strArg(in, "user")
	if id == "" {
		return nil, fmt.Errorf("slack: user is required")
	}
	var res struct {
		User struct {
			ID      string `json:"id"`
			Name    string `json:"name"`
			IsBot   bool   `json:"is_bot"`
			Profile struct {
				RealName string `json:"real_name"`
				Email    string `json:"email"`
				Title    string `json:"title"`
			} `json:"profile"`
		} `json:"user"`
	}
	if err := call(ctx, botToken(cr), "users.info", url.Values{"user": {id}}, &res); err != nil {
		return nil, err
	}
	out := map[string]any{"id": res.User.ID, "handle": res.User.Name, "is_bot": res.User.IsBot}
	if n := res.User.Profile.RealName; n != "" {
		out["name"] = n
	}
	// Email is only present with users:read.email; absent is normal, not an error.
	if e := res.User.Profile.Email; e != "" {
		out["email"] = e
	}
	if t := res.User.Profile.Title; t != "" {
		out["title"] = t
	}
	return out, nil
}

func history(ctx context.Context, cr connectorkit.Credentials, in map[string]any) (any, error) {
	channel := strArg(in, "channel")
	if channel == "" {
		return nil, fmt.Errorf("slack: channel is required")
	}
	limit := intArg(in, "limit", 20)
	if limit > 100 {
		limit = 100
	}

	var res struct {
		Messages []struct {
			User string `json:"user"`
			Text string `json:"text"`
			TS   string `json:"ts"`
			Bot  string `json:"bot_id"`
		} `json:"messages"`
	}
	if err := call(ctx, botToken(cr), "conversations.history",
		url.Values{"channel": {channel}, "limit": {fmt.Sprint(limit)}}, &res); err != nil {
		return nil, err
	}

	out := make([]map[string]any, 0, len(res.Messages))
	for _, m := range res.Messages {
		who := m.User
		if who == "" && m.Bot != "" {
			who = "bot:" + m.Bot
		}
		out = append(out, map[string]any{"from": who, "text": m.Text, "ts": m.TS})
	}
	return map[string]any{"count": len(out), "messages": out}, nil
}

func strArg(in map[string]any, key string) string {
	if v, ok := in[key].(string); ok {
		return v
	}
	return ""
}

func intArg(in map[string]any, key string, def int) int {
	switch n := in[key].(type) {
	case float64:
		if n > 0 {
			return int(n)
		}
	case int:
		if n > 0 {
			return n
		}
	}
	return def
}
