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
	EventTimerFired     EventKind = "timer.fired"
	EventDelegationDone EventKind = "delegation.completed"
)

type Event struct {
	ID        string            `json:"id"`
	Kind      EventKind         `json:"kind"`
	AgentID   string            `json:"agent_id,omitempty"`
	Timestamp time.Time         `json:"timestamp"`
	Payload   map[string]any    `json:"payload"`
	Meta      map[string]string `json:"meta,omitempty"`
	// Seq is the log position, set on read. Zero on an event not yet appended.
	Seq int64 `json:"seq,omitempty"`
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

// Subscriber names. The name IS the durable offset key, so these are constants
// rather than strings at the call site — a typo would silently create a second
// subscriber that replays from the beginning.
const (
	SubLoopSchedule   = "loops.schedule"
	SubLoopEvent      = "loops.event"
	SubLoopWebhook    = "loops.webhook"
	SubLoopTimer      = "loops.timer"
	SubRecipeSchedule = "recipes.schedule"
	SubRecipeTimer    = "recipes.timer"
	SubRecipeEvent    = "recipes.event"
	SubAgentRouter    = "agents.router"
	SubCritical       = "alerts.critical"
)
