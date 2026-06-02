package comms

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/MelloB1989/karmax/internal/bus"
	"github.com/MelloB1989/karmax/internal/store"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

// EventCommsMessage is the bus event kind for incoming communication messages.
const EventCommsMessage bus.EventKind = "comms.message"

// channelEntry pairs a Channel with its owning agent ID.
type channelEntry struct {
	channel Channel
	agentID string
}

// Manager owns all registered communication channels and routes messages
// between them, the event bus, and persistent storage.
type Manager struct {
	channels            map[string]*channelEntry
	lastIncomingTarget  map[string]string // agentID -> last Discord channel ID
	bus                 *bus.Bus
	store               *store.Store
	log                 *zap.Logger
	mu                  sync.RWMutex
}

// NewManager creates a Manager wired to the given bus, store, and logger.
func NewManager(b *bus.Bus, s *store.Store, log *zap.Logger) *Manager {
	return &Manager{
		channels:           make(map[string]*channelEntry),
		lastIncomingTarget: make(map[string]string),
		bus:                b,
		store:              s,
		log:                log,
	}
}

// Register adds a channel to the manager, associating it with the given agent.
func (m *Manager) Register(ch Channel, agentID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if _, exists := m.channels[ch.ID()]; exists {
		return fmt.Errorf("channel %s already registered", ch.ID())
	}

	m.channels[ch.ID()] = &channelEntry{
		channel: ch,
		agentID: agentID,
	}

	m.log.Info("channel registered",
		zap.String("channel_id", ch.ID()),
		zap.String("channel_type", ch.Type()),
		zap.String("agent_id", agentID),
	)
	return nil
}

// StartAll starts every registered channel and spawns a goroutine per channel
// to read incoming messages, persist them, and publish bus events.
func (m *Manager) StartAll(ctx context.Context) error {
	m.mu.RLock()
	defer m.mu.RUnlock()

	for id, entry := range m.channels {
		if err := entry.channel.Start(ctx); err != nil {
			m.log.Error("failed to start channel",
				zap.String("channel_id", id),
				zap.Error(err),
			)
			return fmt.Errorf("start channel %s: %w", id, err)
		}

		m.log.Info("channel started", zap.String("channel_id", id))

		go m.readLoop(ctx, entry)
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
			m.mu.Unlock()

			// Publish to event bus.
			m.bus.Publish(bus.NewEvent(EventCommsMessage, agentID, map[string]any{
				"message_id":       msg.ID,
				"channel_id":       msg.ChannelID,
				"karmax_channel_id": ch.ID(),
				"channel_type":     msg.ChannelType,
				"sender_id":        msg.SenderID,
				"sender_name":      msg.SenderName,
				"content":          msg.Content,
				"direction":        string(msg.Direction),
				"reply_to_id":      msg.ReplyToID,
				"timestamp":        msg.Timestamp,
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

// Get returns a channel by its ID.
func (m *Manager) Get(id string) (Channel, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	entry, ok := m.channels[id]
	if !ok {
		return nil, false
	}
	return entry.channel, true
}

// Send dispatches a text message through the specified channel.
func (m *Manager) Send(channelID, target, content string) error {
	ch, ok := m.Get(channelID)
	if !ok {
		return fmt.Errorf("channel %s not found", channelID)
	}
	return ch.Send(context.Background(), target, content)
}

// SendEmbed dispatches a rich embed through the specified channel.
func (m *Manager) SendEmbed(channelID, target string, embed Embed) error {
	ch, ok := m.Get(channelID)
	if !ok {
		return fmt.Errorf("channel %s not found", channelID)
	}
	return ch.SendEmbed(context.Background(), target, embed)
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

// GetAgentForChannel returns the agent ID associated with the given KARMAX channel ID.
// Returns an empty string if the channel is not registered.
func (m *Manager) GetAgentForChannel(channelID string) string {
	m.mu.RLock()
	defer m.mu.RUnlock()

	entry, ok := m.channels[channelID]
	if !ok {
		return ""
	}
	return entry.agentID
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

// SendToAgent finds all channels associated with the given agent and sends a
// message to each one using the last known Discord channel ID for that agent.
func (m *Manager) SendToAgent(agentID, content string) error {
	channels := m.GetChannelsForAgent(agentID)
	if len(channels) == 0 {
		return fmt.Errorf("no channels registered for agent %s", agentID)
	}

	target := m.GetLastTarget(agentID)
	if target == "" {
		return fmt.Errorf("no known target channel for agent %s", agentID)
	}

	var lastErr error
	for _, ch := range channels {
		if err := ch.Send(context.Background(), target, content); err != nil {
			m.log.Error("failed to send to agent channel",
				zap.String("agent_id", agentID),
				zap.String("channel_id", ch.ID()),
				zap.String("target", target),
				zap.Error(err),
			)
			lastErr = err
		}
	}
	return lastErr
}

// GetLastTarget returns the Discord channel ID where the last incoming message
// was received for the given agent. Returns an empty string if no message has
// been received yet.
func (m *Manager) GetLastTarget(agentID string) string {
	m.mu.RLock()
	defer m.mu.RUnlock()

	return m.lastIncomingTarget[agentID]
}
