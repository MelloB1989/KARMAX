package runtime

import (
	"context"
	"fmt"
	"github.com/MelloB1989/karmax/internal/comms"
	"strings"

	"github.com/MelloB1989/karmax/internal/bus"
	"github.com/MelloB1989/karmax/internal/store"
	"go.uber.org/zap"
)

// Runtime support for the organisational Kit: where a message goes and who
// holds a role.

// sendToChannel posts into a comms channel, threading when asked.
//
// channel may be a registered channel id or a platform-native target such as
// "#eng"; the manager owns that resolution. thread is passed as part of the
// target so a channel that understands threads keeps the conversation in one
// place and one that does not simply posts.
func (rt *KarmaxRuntime) sendToChannel(ctx context.Context, channel, thread, content string) error {
	if strings.TrimSpace(channel) == "" {
		return fmt.Errorf("no channel to send to")
	}
	id, target, err := rt.routeChannel(channel)
	if err != nil {
		return err
	}
	if thread != "" {
		target = target + ":" + thread
	}
	return rt.comms.SendFrom(comms.ProducerLoop, id, target, content)
}

// routeChannel splits "<channel-id>:<target>" into a registered channel and the
// target within it, so a workflow can address any connected chat — Slack,
// Google Chat, Discord, a webhook bridge — by naming it. A destination whose
// prefix is not a registered channel goes to the default channel unchanged,
// which keeps every existing "#eng" and phone-number caller working.
func (rt *KarmaxRuntime) routeChannel(channel string) (string, string, error) {
	channel = strings.TrimSpace(channel)
	if rt.comms.HasChannel(channel) {
		return channel, "", nil
	}
	if i := strings.Index(channel, ":"); i > 0 && rt.comms.HasChannel(channel[:i]) {
		return channel[:i], channel[i+1:], nil
	}
	id, ok := rt.comms.DefaultChannelID()
	if !ok {
		return "", "", fmt.Errorf("no comms channel is connected")
	}
	return id, channel, nil
}

// postToCase posts into a channel, threading when the case already has one, and
// returns the message id so the first message can become the thread.
func (rt *KarmaxRuntime) postToCase(ctx context.Context, channel, thread, text string) (string, error) {
	id, target, err := rt.routeChannel(channel)
	if err != nil {
		return "", err
	}
	return rt.comms.PostThread(ctx, id, target, thread, text)
}

// askRole puts one proposal in front of everyone holding a role.
//
// The proposal is already stored; this is only the asking. Whoever answers
// first settles it, which is what stops an approval waiting on one person's
// return from leave.
func (rt *KarmaxRuntime) askRole(ctx context.Context, role string, members []store.OrgMember, id, title, summary string) {
	body := fmt.Sprintf("*%s*\n%s\n\n_approval %s — first answer decides_", title, summary, id)
	for _, m := range members {
		target := m.Member
		if dm, found, err := rt.store.MemberByExternal("slack", m.Member); err == nil && found {
			target = dm.ExternalID
		}
		if err := rt.sendToChannel(ctx, target, "", body); err != nil {
			rt.log.Warn("could not ask a role member for approval",
				zap.String("role", role), zap.String("member", m.Member), zap.Error(err))
		}
	}
}

// emitCaseState publishes a case's move so a notify workflow can hang off it,
// rather than every workflow having to remember to announce itself.
func (rt *KarmaxRuntime) emitCaseState(caseID, state string) {
	c, found, err := rt.store.CaseByID(caseID)
	if err != nil || !found {
		return
	}
	if err := rt.bus.Publish(bus.Event{
		Kind: bus.EventKind(EventCaseStateChanged),
		Payload: map[string]any{
			"case_id": c.ID, "key": c.Key, "agent": c.Agent,
			"title": c.Title, "state": state,
			"thread_channel": c.ThreadChannel, "thread_ts": c.ThreadTS,
		},
	}); err != nil {
		rt.log.Warn("could not publish a case state change", zap.String("case", caseID), zap.Error(err))
	}
}

// EventCaseStateChanged is what dev-notify style workflows listen on.
const EventCaseStateChanged = "case.state_changed"

