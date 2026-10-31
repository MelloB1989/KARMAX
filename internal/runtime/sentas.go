package runtime

import (
	"fmt"
	"strings"

	"github.com/MelloB1989/karmax/internal/store"
	"github.com/MelloB1989/karmax/internal/tools"
	"github.com/MelloB1989/karmax/internal/tools/builtin"
	"github.com/google/uuid"
)

// recordSentAs is the one recorder every acting tool is given (tools.SentAs).
// "Act freely, tell after": what was said or done as the operator goes into
// the outbound ledger — where every send as the operator is kept, and which
// the agent's per-turn "Sent as you" report reads — with the login it was
// done as, and into the app. The app notice is quiet: the turn's one report
// is what reaches WhatsApp, rather than a separate message per act.
func recordSentAs(s *store.Store, agentID string) tools.SentRecorder {
	return func(sent tools.SentAs) {
		title, body := sentAsNotice(sent)
		builtin.PushAppNotificationQuiet(s, agentID, "info", title, body)
		if s == nil {
			return
		}
		// Untagged producer: this is the agent's own send, like comms.send.
		_ = s.SaveChannelMessage(store.StoredChannelMessage{
			ID: uuid.New().String(), ChannelID: sent.To, ChannelType: sent.Service,
			SenderName: sent.Account, Direction: "outbound", Content: sent.Content(),
			Metadata: sent.URL,
		})
	}
}

// sentAsNotice is what the operator's app is told after KARMAX acted as them.
func sentAsNotice(s tools.SentAs) (title, body string) {
	did := strings.TrimSpace(s.Action)
	if did == "" {
		did = "wrote to"
	}
	title = fmt.Sprintf("On %s as %s: %s %s", tools.ServiceName(s.Service), s.Account, did, s.To)
	body = s.URL
	if said := strings.Join(strings.Fields(s.Text), " "); said != "" {
		if r := []rune(said); len(r) > 280 {
			said = string(r[:280]) + "…"
		}
		body = strings.TrimSpace("“" + said + "”\n" + s.URL)
	}
	if body == "" {
		body = title // a notice needs a body to be kept
	}
	return title, body
}
