package runtime

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/MelloB1989/karmax/internal/store"
	"github.com/MelloB1989/karmax/internal/tools/builtin"
	"github.com/MelloB1989/karmax/pkg/loopkit"
	"go.uber.org/zap"
)

const (
	mirrorJevTimeout = 15 * time.Second
	mirrorBodyLimit  = 600
)

// operatorMirror carries app notifications and approvals to the operator's WhatsApp DM.
type operatorMirror struct {
	log    *zap.Logger
	send   func(text string) error
	decide func(ctx context.Context, state any, q loopkit.Questions) (*loopkit.Decision, error)

	mu       sync.Mutex
	lastSent time.Time
	inFlight map[string]int
}

func newOperatorMirror(log *zap.Logger, send func(string) error,
	decide func(context.Context, any, loopkit.Questions) (*loopkit.Decision, error)) *operatorMirror {
	return &operatorMirror{log: log, send: send, decide: decide, inFlight: map[string]int{}}
}

// Hook is the builtin mirror hook; it never blocks the caller.
func (m *operatorMirror) Hook(ev builtin.MirrorEvent) {
	go m.handle(ev)
}

func (m *operatorMirror) handle(ev builtin.MirrorEvent) {
	if m.send == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), mirrorJevTimeout)
	defer cancel()

	if !m.worthWhatsApp(ctx, ev) {
		return
	}
	text := mirrorText(ev)

	m.mu.Lock()
	m.inFlight[text]++
	m.mu.Unlock()
	err := m.send(text)
	m.mu.Lock()
	if m.inFlight[text]--; m.inFlight[text] <= 0 {
		delete(m.inFlight, text)
	}
	if err == nil {
		m.lastSent = time.Now()
	}
	m.mu.Unlock()
	if err != nil {
		m.log.Warn("could not mirror to the operator's WhatsApp", zap.String("source", ev.Source), zap.Error(err))
	}
}

// worthWhatsApp asks Jev; any failure to get an answer means send.
func (m *operatorMirror) worthWhatsApp(ctx context.Context, ev builtin.MirrorEvent) bool {
	if m.decide == nil {
		return true
	}
	m.mu.Lock()
	since := -1
	if !m.lastSent.IsZero() {
		since = int(time.Since(m.lastSent).Seconds())
	}
	m.mu.Unlock()

	state := map[string]any{
		"kind":                               ev.Kind,
		"title":                              ev.Title,
		"body":                               clip(ev.Body, mirrorBodyLimit),
		"source":                             ev.Source,
		"proposal_id":                        ev.ProposalID,
		"seconds_since_last_whatsapp_mirror": since,
	}
	dec, err := m.decide(ctx, state, loopkit.Questions{
		"deliver": loopkit.Choice(
			"This item is already in the operator's phone app. Should it also be sent to the operator's WhatsApp DM right now? An approval waits on a person, so it nearly always should be. A notification should be when the operator would want to know before they next open the app; a routine status line, a repeat of something just sent, or noise should not.",
			map[string]string{
				"whatsapp_now": "the operator should see this in WhatsApp now",
				"app_only":     "leave it in the app; WhatsApp would add noise",
			}),
	})
	if err != nil || dec == nil {
		return true
	}
	choice, _ := dec.Choice("deliver")
	return choice != "app_only"
}

// Notifier wraps the proactive WhatsApp-to-app push so the mirror's own sends are not echoed back.
func (m *operatorMirror) Notifier(push func(target, content string)) func(target, content string) {
	return func(target, content string) {
		m.mu.Lock()
		own := m.inFlight[content] > 0
		m.mu.Unlock()
		if own {
			return
		}
		push(target, content)
	}
}

func mirrorText(ev builtin.MirrorEvent) string {
	var b strings.Builder
	if ev.Kind == builtin.MirrorApproval {
		b.WriteString("Approval needed: ")
	}
	b.WriteString(strings.TrimSpace(ev.Title))
	if body := strings.TrimSpace(clip(ev.Body, mirrorBodyLimit)); body != "" {
		b.WriteString("\n\n" + body)
	}
	if ev.Kind == builtin.MirrorApproval {
		b.WriteString("\n\nProposal id: " + ev.ProposalID + "\nReply in this chat to approve or reject it.")
	}
	return b.String()
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// installOperatorMirror wires the mirror and re-registers the proactive notifier behind its guard.
func (rt *KarmaxRuntime) installOperatorMirror(s *store.Store, agentID string, send func(string) error) {
	m := newOperatorMirror(rt.log.Named("opmirror"), send, rt.decide)
	builtin.SetMirrorHook(m.Hook)
	rt.comms.SetProactiveNotifier(m.Notifier(func(target, content string) {
		builtin.PushAppNotificationQuiet(s, agentID, "update", "Sent to "+target, clip(content, 240))
	}))
}
