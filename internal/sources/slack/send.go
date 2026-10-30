package slack

import (
	"context"
	"fmt"
	"net/url"
	"strings"
)

// Sent is a message KARMAX posted as the operator.
type Sent struct {
	Workspace string
	Channel   string
	TS        string
	URL       string
}

// Send posts text as the operator — their own account, not a bot — to a
// channel or DM, inside threadTS when it is a thread reply. workspace is an
// account the source publishes under ("northwind-labs"), a team id, or the
// integration id; it may be left empty when only one workspace is connected.
func (s *Source) Send(ctx context.Context, wss []Workspace, workspace, channel, threadTS, text string) (Sent, error) {
	channel, text = strings.TrimSpace(channel), strings.TrimSpace(text)
	if channel == "" || text == "" {
		return Sent{}, fmt.Errorf("slack: a channel and the text are required")
	}
	t, err := s.teamFor(ctx, wss, strings.TrimSpace(workspace))
	if err != nil {
		return Sent{}, err
	}
	form := url.Values{"channel": {channel}, "text": {text}}
	if threadTS = strings.TrimSpace(threadTS); threadTS != "" {
		form.Set("thread_ts", threadTS)
	}
	var out struct {
		Channel string `json:"channel"`
		TS      string `json:"ts"`
	}
	if err := s.call(ctx, t.token, "chat.postMessage", form, &out); err != nil {
		return Sent{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	ch := firstNonEmpty(out.Channel, channel)
	return Sent{Workspace: t.account, Channel: ch, TS: out.TS, URL: permalink(t.base, ch, out.TS, threadTS)}, nil
}

// teamFor finds the workspace a send goes through.
func (s *Source) teamFor(ctx context.Context, wss []Workspace, name string) (*team, error) {
	good, off := usable(wss)
	if len(good) == 0 {
		return nil, fmt.Errorf("%s", strings.Join(off, "\n"))
	}
	var teams []*team
	for _, w := range good {
		t := s.team(w)
		if err := s.identify(ctx, t); err != nil {
			return nil, err
		}
		teams = append(teams, t)
	}
	if name == "" {
		if len(teams) == 1 {
			return teams[0], nil
		}
		return nil, fmt.Errorf("slack: %d workspaces are connected; say which (%s)", len(teams), accounts(teams))
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, t := range teams {
		for _, n := range []string{t.account, t.id, t.integration, t.name} {
			if n != "" && strings.EqualFold(n, name) {
				return t, nil
			}
		}
	}
	return nil, fmt.Errorf("slack: no connected workspace is called %q (connected: %s)", name, accounts(teams))
}

func accounts(teams []*team) string {
	var names []string
	for _, t := range teams {
		names = append(names, t.label())
	}
	return strings.Join(names, ", ")
}
