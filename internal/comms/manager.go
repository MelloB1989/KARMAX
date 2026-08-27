package comms

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/MelloB1989/karmax/internal/bus"
	"github.com/MelloB1989/karmax/internal/store"
	"github.com/MelloB1989/karmax/pkg/karmahelper"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

// EventCommsMessage is the bus event kind for incoming communication messages.
const EventCommsMessage bus.EventKind = "comms.message"

// EventApprovalDecision is the bus event kind for an interactive approve/
// reject decision made on a channel's own UI (Slack's Approve/Reject buttons
// and anything else that reports one the same way).
const EventApprovalDecision bus.EventKind = "comms.approval_decision"

// Decision is one approve/reject choice a person made on a channel's
// interactive UI, reported back with who decided — the point of asking a
// role rather than a person is that the first answer wins, so the decider's
// identity travels with the decision, not just the outcome.
type Decision struct {
	ProposalID  string
	Approved    bool
	DeciderID   string // channel-native user id, e.g. a Slack user id
	DeciderName string
	ChannelID   string // the KARMAX channel id that reported it
	At          time.Time
}

// DecisionSource is implemented by channels that can report interactive
// decisions. Optional — most channels have no interactive UI — so it's
// checked with a type assertion rather than added to the Channel interface.
type DecisionSource interface {
	Decisions() <-chan Decision
}

// channelEntry pairs a Channel with its owning agent ID.
type channelEntry struct {
	channel Channel
	agentID string
	dnd     bool
}

type ChannelOptions struct {
	DND bool
}

// Manager owns all registered communication channels and routes messages
// between them, the event bus, and persistent storage.
type Manager struct {
	channels            map[string]*channelEntry
	lastIncomingTarget  map[string]string            // agentID -> last Discord channel ID
	lastIncomingTargets map[string]map[string]string // agentID -> KARMAX channel ID -> target
	operatorTargets     map[string]bool              // normalized targets that are the operator (never "proactive")
	proactiveNotify     func(target, content string) // fired when a message is sent to a NON-operator target
	bus                 *bus.Log
	store               *store.Store
	log                 *zap.Logger
	guard               *sendGuard
	mu                  sync.RWMutex
}

// NewManager creates a Manager wired to the given bus, store, and logger.
func NewManager(b *bus.Log, s *store.Store, log *zap.Logger) *Manager {
	return &Manager{
		channels:            make(map[string]*channelEntry),
		lastIncomingTarget:  make(map[string]string),
		lastIncomingTargets: make(map[string]map[string]string),
		operatorTargets:     make(map[string]bool),
		guard:               newSendGuard(),
		bus:                 b,
		store:               s,
		log:                 log,
	}
}

// SetProactiveNotifier registers a callback fired after a message is sent to a
// target that is NOT the operator (a proactive, act-and-inform notification).
func (m *Manager) SetProactiveNotifier(fn func(target, content string)) {
	m.mu.Lock()
	m.proactiveNotify = fn
	m.mu.Unlock()
}

// RegisterOperatorTarget marks a target (phone/JID/chat) as the operator's own,
// so messages to it are treated as replies, not proactive outbound.
func (m *Manager) RegisterOperatorTarget(target string) {
	t := normalizeTarget(target)
	if t == "" {
		return
	}
	m.mu.Lock()
	m.operatorTargets[t] = true
	m.mu.Unlock()
}

// isOperatorTarget reports whether a send target is the operator (so it should
// NOT trigger a proactive "sent" notification). Empty target = a reply/default
// (operator-facing). Also matches any chat the operator has messaged from.
func (m *Manager) isOperatorTarget(target string) bool {
	t := normalizeTarget(target)
	if t == "" {
		return true
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.operatorTargets[t] {
		return true
	}
	for _, byChannel := range m.lastIncomingTargets {
		for _, in := range byChannel {
			if normalizeTarget(in) == t {
				return true
			}
		}
	}
	return false
}

// normalizeTarget reduces a target to comparable digits/id, stripping any
// "@domain" and ":device" suffix.
func normalizeTarget(s string) string {
	s = strings.TrimSpace(strings.ToLower(s))
	if i := strings.IndexAny(s, "@:"); i >= 0 {
		s = s[:i]
	}
	return s
}

// Register adds a channel to the manager, associating it with the given agent.
func (m *Manager) Register(ch Channel, agentID string) error {
	return m.RegisterWithOptions(ch, agentID, ChannelOptions{})
}

// RegisterWithOptions adds a channel with runtime behavior flags.
func (m *Manager) RegisterWithOptions(ch Channel, agentID string, opts ChannelOptions) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if _, exists := m.channels[ch.ID()]; exists {
		return fmt.Errorf("channel %s already registered", ch.ID())
	}

	m.channels[ch.ID()] = &channelEntry{
		channel: ch,
		agentID: agentID,
		dnd:     opts.DND,
	}

	m.log.Info("channel registered",
		zap.String("channel_id", ch.ID()),
		zap.String("channel_type", ch.Type()),
		zap.String("agent_id", agentID),
		zap.Bool("dnd", opts.DND),
	)
	return nil
}

// StartAll starts every registered channel and spawns a goroutine per channel
// to read incoming messages, persist them, and publish bus events.
func (m *Manager) StartAll(ctx context.Context) error {
	entries := m.snapshotEntries()

	var failures []string
	for id, entry := range entries {
		if err := entry.channel.Start(ctx); err != nil {
			m.log.Error("failed to start channel",
				zap.String("channel_id", id),
				zap.Error(err),
			)
			_ = m.store.UpdateChannelStatus(id, "failed")
			failures = append(failures, fmt.Sprintf("%s: %v", id, err))
			continue
		}

		_ = m.store.UpdateChannelStatus(id, "connected")
		m.log.Info("channel started", zap.String("channel_id", id))

		go m.readLoop(ctx, entry)
		if ds, ok := entry.channel.(DecisionSource); ok {
			go m.decisionLoop(ctx, ds)
		}
	}

	if len(failures) > 0 {
		err := fmt.Errorf("failed to start comms channels: %s", strings.Join(failures, "; "))
		for id, entry := range entries {
			for _, failure := range failures {
				if strings.HasPrefix(failure, id+":") {
					m.publishCritical(entry.agentID, id, "communication channel failed to start", map[string]any{
						"error": failure,
					})
					_ = m.AlertAlternative(entry.agentID, id, "Critical KARMAX channel failure: "+failure)
				}
			}
		}
		return err
	}

	return nil
}

// readLoop drains incoming messages from a channel, persists each one, and
// publishes a bus event.
func (m *Manager) readLoop(ctx context.Context, entry *channelEntry) {
	ch := entry.channel
	agentID := entry.agentID

	for {
		select {
		case <-ctx.Done():
			return
		case msg, ok := <-ch.IncomingMessages():
			if !ok {
				m.log.Info("incoming message channel closed",
					zap.String("channel_id", ch.ID()),
				)
				return
			}

			if msg.ID == "" {
				msg.ID = uuid.New().String()
			}

			// Persist to store.
			metaJSON, _ := json.Marshal(msg.Metadata)
			if err := m.store.SaveChannelMessage(store.StoredChannelMessage{
				ID:          msg.ID,
				ChannelID:   msg.ChannelID,
				ChannelType: msg.ChannelType,
				SenderID:    msg.SenderID,
				SenderName:  msg.SenderName,
				Direction:   string(msg.Direction),
				Content:     msg.Content,
				ReplyToID:   msg.ReplyToID,
				Metadata:    string(metaJSON),
				CreatedAt:   msg.Timestamp,
			}); err != nil {
				m.log.Error("failed to persist channel message",
					zap.String("channel_id", ch.ID()),
					zap.String("message_id", msg.ID),
					zap.Error(err),
				)
			}

			// Record the last incoming Discord channel ID for this agent.
			m.mu.Lock()
			m.lastIncomingTarget[agentID] = msg.ChannelID
			if m.lastIncomingTargets[agentID] == nil {
				m.lastIncomingTargets[agentID] = make(map[string]string)
			}
			m.lastIncomingTargets[agentID][ch.ID()] = msg.ChannelID
			m.mu.Unlock()

			// Surface metadata fields the proactive proxy uses for routing: group
			// vs 1:1, chat display name, and whether the bot is being directly
			// addressed (@-mentioned or replied-to) — the latter computed
			// generically by wacli from its own identity.
			isGroup, _ := msg.Metadata["is_group"].(bool)
			chatName, _ := msg.Metadata["chat_name"].(string)
			mentionsMe, _ := msg.Metadata["mentions_me"].(bool)
			quotedIsFromMe, _ := msg.Metadata["quoted_is_from_me"].(bool)
			mentionCount, _ := msg.Metadata["mention_count"].(int)
			wacliMessageID, _ := msg.Metadata["wacli_message_id"].(string)

			// Publish to event bus.
			m.bus.Publish(bus.NewEvent(EventCommsMessage, agentID, map[string]any{
				"message_id":        msg.ID,
				"channel_id":        msg.ChannelID,
				"karmax_channel_id": ch.ID(),
				"channel_type":      msg.ChannelType,
				"sender_id":         msg.SenderID,
				"sender_name":       msg.SenderName,
				"content":           msg.Content,
				"direction":         string(msg.Direction),
				"reply_to_id":       msg.ReplyToID,
				"timestamp":         msg.Timestamp,
				"is_group":          isGroup,
				"chat_name":         chatName,
				"mentions_me":       mentionsMe,
				"quoted_is_from_me": quotedIsFromMe,
				"mention_count":     mentionCount,
				// The originating WhatsApp message id, so a loop can reply
				// quoting the exact message that triggered it.
				"wacli_message_id": wacliMessageID,
			}))
		}
	}
}

// decisionLoop drains interactive decisions from a channel (Slack's Approve/
// Reject buttons and anything reporting the same way) and publishes each one
// to the bus — the rest of KARMAX learns about a decision the same way it
// learns about anything else that happened on a channel.
func (m *Manager) decisionLoop(ctx context.Context, ds DecisionSource) {
	for {
		select {
		case <-ctx.Done():
			return
		case d, ok := <-ds.Decisions():
			if !ok {
				return
			}
			m.bus.Publish(bus.NewEvent(EventApprovalDecision, "", map[string]any{
				"proposal_id":  d.ProposalID,
				"approved":     d.Approved,
				"decider_id":   d.DeciderID,
				"decider_name": d.DeciderName,
				"channel_id":   d.ChannelID,
				"at":           d.At,
			}))
		}
	}
}

// StopAll stops every registered channel.
func (m *Manager) StopAll() {
	m.mu.RLock()
	defer m.mu.RUnlock()

	for id, entry := range m.channels {
		if err := entry.channel.Stop(); err != nil {
			m.log.Error("failed to stop channel",
				zap.String("channel_id", id),
				zap.Error(err),
			)
		}
	}
}

// Send dispatches a text message through the specified channel. When the target
// is someone OTHER than the operator, it fires the proactive notifier so the
// operator sees every outbound message KARMAX sends on their behalf.
func (m *Manager) Send(channelID, target, content string) error {
	err := m.send(context.Background(), channelID, target, content, true)
	if err != nil {
		return err
	}
	m.mu.RLock()
	notify := m.proactiveNotify
	m.mu.RUnlock()
	if notify != nil && !m.isOperatorTarget(target) {
		notify(target, content)
	}
	return nil
}

// Threader is a channel that understands threads. Slack does; WhatsApp does
// not, and a channel that cannot thread simply posts.
type Threader interface {
	PostThread(ctx context.Context, channel, threadTS, text string) (string, error)
}

// PostThread posts into a thread and returns the message's own id, which is
// what lets the FIRST message on a case become the thread every later one joins.
//
// A channel that does not thread falls back to an ordinary send and returns an
// empty id — the caller then simply has no thread to bind, rather than failing.
func (m *Manager) PostThread(ctx context.Context, channelID, target, thread, content string) (string, error) {
	m.mu.RLock()
	entry, ok := m.channels[channelID]
	m.mu.RUnlock()
	if !ok {
		return "", fmt.Errorf("no channel %q", channelID)
	}
	if t, canThread := entry.channel.(Threader); canThread {
		return t.PostThread(ctx, target, thread, content)
	}
	return "", m.Send(channelID, target, content)
}

// HasChannel reports whether id names a registered channel, so callers can
// tell a transport name from a recipient.
func (m *Manager) HasChannel(id string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	_, ok := m.channels[id]
	return ok
}

// DefaultChannelID picks the channel to use when a caller doesn't specify one:
// the sole channel if only one is registered, otherwise the first WhatsApp
// channel, otherwise any.
func (m *Manager) DefaultChannelID() (string, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if len(m.channels) == 0 {
		return "", false
	}
	var any string
	for id, entry := range m.channels {
		if entry.channel.Type() == "whatsapp" {
			return id, true
		}
		any = id
	}
	return any, true
}

// FindChannelIDByType returns the ID of the first registered channel of the
// given type (e.g. "whatsapp"), so callers never hardcode channel IDs.
func (m *Manager) FindChannelIDByType(channelType string) (string, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for id, entry := range m.channels {
		if entry.channel.Type() == channelType {
			return id, true
		}
	}
	return "", false
}

// List returns all registered channels.
func (m *Manager) List() []Channel {
	m.mu.RLock()
	defer m.mu.RUnlock()

	out := make([]Channel, 0, len(m.channels))
	for _, entry := range m.channels {
		out = append(out, entry.channel)
	}
	return out
}

// GetChannelsForAgent returns all channels registered to the given agent ID.
func (m *Manager) GetChannelsForAgent(agentID string) []Channel {
	m.mu.RLock()
	defer m.mu.RUnlock()

	var out []Channel
	for _, entry := range m.channels {
		if entry.agentID == agentID {
			out = append(out, entry.channel)
		}
	}
	return out
}

func (m *Manager) ChannelDND(channelID string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()

	entry, ok := m.channels[channelID]
	return ok && entry.dnd
}

func (m *Manager) RequestEscalation(agentID, primaryChannelID, content string) error {
	content = karmahelper.CleanContent(content)
	if strings.TrimSpace(content) == "" {
		return fmt.Errorf("refusing to send empty escalation request")
	}

	if primaryChannelID != "" {
		target := m.lastTargetFor(agentID, primaryChannelID)
		if err := m.send(context.Background(), primaryChannelID, target, content, false); err == nil {
			return nil
		}
	}

	return m.AlertAlternative(agentID, primaryChannelID, content)
}

func (m *Manager) AlertAlternative(agentID, primaryChannelID, content string) error {
	content = karmahelper.CleanContent(content)
	if strings.TrimSpace(content) == "" {
		return fmt.Errorf("refusing to send empty alternative alert")
	}

	entries := m.snapshotEntries()
	var lastErr error
	for id, entry := range entries {
		if agentID != "" && entry.agentID != agentID {
			continue
		}
		if id == primaryChannelID || entry.dnd {
			continue
		}
		targetAgentID := agentID
		if targetAgentID == "" {
			targetAgentID = entry.agentID
		}
		target := m.lastTargetFor(targetAgentID, id)
		if target == "" && entry.channel.Type() != "whatsapp" {
			// Skip channels with no known target, unless the channel can
			// self-route (WhatsApp falls back to its configured target_chat).
			continue
		}
		if err := m.send(context.Background(), id, target, content, false); err != nil {
			lastErr = err
			continue
		}
		return nil
	}

	if lastErr != nil {
		return lastErr
	}
	return fmt.Errorf("no alternative channel available for agent %s", agentID)
}

func (m *Manager) send(ctx context.Context, channelID, target, content string, alertOnFailure bool) error {
	content = karmahelper.CleanContent(content)
	if strings.TrimSpace(content) == "" {
		return fmt.Errorf("refusing to send empty message")
	}

	entry, ok := m.getEntry(channelID)
	if !ok {
		return fmt.Errorf("channel %s not found", channelID)
	}
	if entry.dnd {
		err := fmt.Errorf("channel %s is in DND mode", channelID)
		if alertOnFailure {
			m.publishCritical(entry.agentID, channelID, "communication channel is in DND mode", map[string]any{
				"target": target,
			})
			_ = m.AlertAlternative(entry.agentID, channelID, "KARMAX needs attention, but the primary channel is in DND mode.")
		}
		return err
	}

	// Say a thing once. This is the only place every outbound message passes
	// through, so it is the only place that can see a repeat regardless of
	// which loop, tool or recipe produced it.
	if why, dup := m.recentlySaid(target, content); dup {
		m.log.Info("refusing a duplicate message",
			zap.String("target", target),
			zap.String("reason", why),
			zap.String("content", truncateForLog(content)),
		)
		return fmt.Errorf("%w: %s", ErrDuplicateSend, why)
	}
	guardKey := target + "\x00" + normalizeMessage(content)
	if !m.guard.reserve(guardKey, time.Now()) {
		m.log.Info("refusing a duplicate message already in flight",
			zap.String("target", target),
			zap.String("content", truncateForLog(content)),
		)
		return fmt.Errorf("%w: an identical message is being sent right now", ErrDuplicateSend)
	}
	defer m.guard.release(guardKey)

	if err := entry.channel.Send(ctx, target, content); err != nil {
		// A bad address is the caller's mistake and is returned to them to fix.
		// Alerting on it woke the operator over a tool argument, and — because
		// the alert is itself a message — invited the same failure on the way
		// out. Both shapes count: a name that matched nothing and a name that
		// matched several are equally answerable by whoever wrote it.
		if alertOnFailure && !isAddressingMistake(err) {
			m.publishCritical(entry.agentID, channelID, "communication channel send failed", map[string]any{
				"target": target,
				"error":  err.Error(),
			})
			_ = m.AlertAlternative(entry.agentID, channelID, "Critical KARMAX send failure on "+channelID+": "+err.Error())
		}
		return err
	}

	msgID := uuid.New().String()
	metaJSON, _ := json.Marshal(map[string]any{
		"karmax_channel_id": channelID,
	})
	if err := m.store.SaveChannelMessage(store.StoredChannelMessage{
		ID:          msgID,
		ChannelID:   target,
		ChannelType: entry.channel.Type(),
		Direction:   string(Outbound),
		Content:     content,
		Metadata:    string(metaJSON),
	}); err != nil {
		m.log.Error("failed to persist outbound channel message",
			zap.String("channel_id", channelID),
			zap.String("message_id", msgID),
			zap.Error(err),
		)
	}

	m.bus.Publish(bus.NewEvent(bus.EventCommsSent, entry.agentID, map[string]any{
		"message_id":        msgID,
		"channel_id":        target,
		"karmax_channel_id": channelID,
		"channel_type":      entry.channel.Type(),
		"direction":         string(Outbound),
		"content":           content,
	}))

	return nil
}

func (m *Manager) getEntry(channelID string) (*channelEntry, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	entry, ok := m.channels[channelID]
	return entry, ok
}

func (m *Manager) snapshotEntries() map[string]*channelEntry {
	m.mu.RLock()
	defer m.mu.RUnlock()

	entries := make(map[string]*channelEntry, len(m.channels))
	for id, entry := range m.channels {
		entries[id] = entry
	}
	return entries
}

func (m *Manager) lastTargetFor(agentID, channelID string) string {
	m.mu.RLock()
	defer m.mu.RUnlock()

	if byChannel := m.lastIncomingTargets[agentID]; byChannel != nil {
		return byChannel[channelID]
	}
	return ""
}

func (m *Manager) publishCritical(agentID, channelID, message string, fields map[string]any) {
	payload := map[string]any{
		"severity":                    "critical",
		"message":                     message,
		"agent_id":                    agentID,
		"karmax_channel_id":           channelID,
		"alternative_alert_attempted": true,
	}
	for k, v := range fields {
		payload[k] = v
	}
	m.bus.Publish(bus.NewEvent(bus.EventSystemCritical, agentID, payload))
}

// recentlySaid reports whether this message has just gone to this target.
//
// The store is the source of truth rather than an in-process cache, so a
// restart does not wipe the memory of what was said — the daemon restarting
// between two halves of a duplicate pair is exactly when it is least able to
// notice.
func (m *Manager) recentlySaid(target, content string) (string, bool) {
	if m.store == nil {
		return "", false
	}
	rows, err := m.store.ListChannelMessages(target, 15)
	if err != nil {
		// Never block a send because the history could not be read: silence is
		// worse than a repeat.
		m.log.Warn("could not check for a duplicate send", zap.Error(err))
		return "", false
	}
	recent := make([]pastMessage, 0, len(rows))
	for _, r := range rows {
		if r.Direction != string(Outbound) {
			continue
		}
		recent = append(recent, pastMessage{Content: r.Content, At: r.CreatedAt})
	}
	return isRepeat(content, recent, time.Now())
}

func truncateForLog(s string) string {
	if len(s) > 120 {
		return s[:120] + "…"
	}
	return s
}
