package bus

import (
	"time"

	"github.com/google/uuid"
)

type EventKind string

const (
	EventAgentStarted   EventKind = "agent.started"
	EventAgentStopped   EventKind = "agent.stopped"
	EventAgentFailed    EventKind = "agent.failed"
	EventAgentMessage   EventKind = "agent.message"
	EventWebhookFired   EventKind = "webhook.fired"
	EventScheduledJob   EventKind = "scheduler.job"
	EventMemoryUpdated  EventKind = "memory.updated"
	EventToolCalled     EventKind = "tool.called"
	EventToolResult     EventKind = "tool.result"
	EventUserDefined    EventKind = "user.defined"
	EventCommsMessage   EventKind = "comms.message"
	EventCommsSent      EventKind = "comms.sent"
	EventSystemCritical EventKind = "system.critical"
)

type Event struct {
	ID        string            `json:"id"`
	Kind      EventKind         `json:"kind"`
	AgentID   string            `json:"agent_id,omitempty"`
	Timestamp time.Time         `json:"timestamp"`
	Payload   map[string]any    `json:"payload"`
	Meta      map[string]string `json:"meta,omitempty"`
}

func NewEvent(kind EventKind, agentID string, payload map[string]any) Event {
	return Event{
		ID:        uuid.New().String(),
		Kind:      kind,
		AgentID:   agentID,
		Timestamp: time.Now(),
		Payload:   payload,
	}
}

type Subscription struct {
	ID      string
	Filters []EventKind
	Ch      chan Event
}

func (s *Subscription) matches(kind EventKind) bool {
	if len(s.Filters) == 0 {
		return true
	}
	for _, f := range s.Filters {
		if f == kind {
			return true
		}
	}
	return false
}
