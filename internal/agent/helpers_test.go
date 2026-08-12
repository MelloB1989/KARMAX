package agent

import (
	"strings"
	"testing"
	"time"

	"github.com/MelloB1989/karmax/internal/bus"
	"github.com/MelloB1989/karmax/internal/memory"
)

func TestBuildPromptFromEvent_CommsMessage(t *testing.T) {
	evt := bus.Event{
		ID:        "test-evt-1",
		Kind:      bus.EventCommsMessage,
		AgentID:   "agent-main",
		Timestamp: time.Now(),
		Payload: map[string]any{
			"content":           "Hello KARMAX",
			"sender":            "testuser",
			"channel_id":        "12345",
			"karmax_channel_id": "discord-main",
		},
	}

	prompt := buildPromptFromEvent(evt, nil)

	if !strings.Contains(prompt, "## Current Task") {
		t.Error("prompt should contain '## Current Task' section")
	}
	// A message is rendered as plain instruction text, NOT an event JSON dump —
	// making the model dig the message out of JSON is what produced the
	// "empty trigger / nothing to act on" replies.
	if !strings.Contains(prompt, "Hello KARMAX") {
		t.Error("prompt should contain the message content from payload")
	}
	if !strings.Contains(prompt, "12345") {
		t.Error("prompt should contain the chat id so the agent can reply to it")
	}
	if strings.Contains(prompt, "```json") {
		t.Error("comms messages must not be rendered as raw JSON")
	}
}

func TestBuildPromptFromEvent_TrackerEvent(t *testing.T) {
	evt := bus.Event{
		ID:      "test-evt-3",
		Kind:    "tracker.event",
		AgentID: "agent-main",
		Payload: map[string]any{
			"summary":  "github · MelloB1989/karmax: #42 opened — wacli drops messages",
			"url":      "https://github.com/MelloB1989/karmax/issues/42",
			"assignee": "nikhil",
			"body":     "Sometimes the webhook never fires.",
		},
	}

	prompt := buildPromptFromEvent(evt, nil)

	if !strings.Contains(prompt, "#42 opened") || !strings.Contains(prompt, "nikhil") {
		t.Errorf("prompt should summarise the tracker event, got:\n%s", prompt)
	}
	if strings.Contains(prompt, "```json") {
		t.Error("tracker events must not be rendered as raw JSON")
	}
	// Silence must be the stated default, or every webhook becomes a notification.
	if !strings.Contains(prompt, "Do not send a message") {
		t.Error("prompt should tell the agent that no action is the usual outcome")
	}
}

func TestBuildPromptFromEvent_WithRecentMemory(t *testing.T) {
	evt := bus.Event{
		ID:      "test-evt-2",
		Kind:    bus.EventScheduledJob,
		AgentID: "agent-main",
		Payload: map[string]any{
			"job": "health-check",
		},
	}

	recentMem := []memory.MemoryEntry{
		{
			Role:      "user",
			Content:   "Previous conversation about deployment",
			CreatedAt: time.Now().Add(-5 * time.Minute),
		},
		{
			Role:      "assistant",
			Content:   "Deployment completed successfully",
			CreatedAt: time.Now().Add(-4 * time.Minute),
		},
	}

	prompt := buildPromptFromEvent(evt, recentMem)

	if !strings.Contains(prompt, "## Recent Context") {
		t.Error("prompt should contain '## Recent Context' when memories are provided")
	}
	if !strings.Contains(prompt, "Previous conversation about deployment") {
		t.Error("prompt should contain recent memory content")
	}
	if !strings.Contains(prompt, "Deployment completed successfully") {
		t.Error("prompt should contain recent memory assistant content")
	}
}

func TestBuildPromptFromEvent_NoMemory(t *testing.T) {
	evt := bus.Event{
		ID:      "test-evt-3",
		Kind:    bus.EventWebhookFired,
		AgentID: "agent-main",
		Payload: nil,
	}

	prompt := buildPromptFromEvent(evt, nil)

	if strings.Contains(prompt, "## Recent Context") {
		t.Error("prompt should NOT contain '## Recent Context' when no memories")
	}
	if !strings.Contains(prompt, "## Current Task") {
		t.Error("prompt should contain '## Current Task'")
	}
}

func TestBuildPromptFromEvent_NilPayload(t *testing.T) {
	evt := bus.Event{
		ID:      "test-evt-4",
		Kind:    bus.EventUserDefined,
		AgentID: "agent-main",
		Payload: nil,
	}

	prompt := buildPromptFromEvent(evt, nil)

	if !strings.Contains(prompt, "Event: user.defined") {
		t.Errorf("prompt should contain event kind, got: %s", prompt)
	}
	// Should NOT contain json block when payload is nil
	if strings.Contains(prompt, "```json") {
		t.Error("prompt should not have JSON block when payload is nil")
	}
}

func TestTruncateStr(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		maxLen   int
		expected string
	}{
		{"short string", "hello", 10, "hello"},
		{"exact length", "hello", 5, "hello"},
		{"too long", "hello world", 5, "hello..."},
		{"empty", "", 5, ""},
		{"one char max", "hello", 1, "h..."},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := truncateStr(tt.input, tt.maxLen)
			if got != tt.expected {
				t.Errorf("truncateStr(%q, %d) = %q, want %q", tt.input, tt.maxLen, got, tt.expected)
			}
		})
	}
}

// A job the agent scheduled for itself must arrive as its instruction, not as a
// JSON dump. scheduler.add stores that instruction under "task", and reading
// only "prompt" is what made these fire as contentless events.
func TestBuildPromptFromEvent_ScheduledTaskCarriesItsInstruction(t *testing.T) {
	evt := bus.Event{
		ID:      "test-evt-task",
		Kind:    bus.EventScheduledJob,
		AgentID: "nexus",
		Payload: map[string]any{
			"job_name": "CampX follow-up",
			"payload":  map[string]any{"task": "Ask Siva where the VAPT reports stand"},
		},
	}

	prompt := buildPromptFromEvent(evt, nil)

	if !strings.Contains(prompt, "Ask Siva where the VAPT reports stand") {
		t.Errorf("the instruction was dropped, got: %s", prompt)
	}
	if !strings.Contains(prompt, "CampX follow-up") {
		t.Errorf("the job name should label the task, got: %s", prompt)
	}
	if strings.Contains(prompt, "```json") {
		t.Errorf("an instruction-carrying job should not be dumped as JSON, got: %s", prompt)
	}
}

// An event with nothing to act on must not push the agent into messaging the
// operator about it.
func TestBuildPromptFromEvent_ContentlessEventPermitsSilence(t *testing.T) {
	evt := bus.Event{
		ID:      "test-evt-empty",
		Kind:    bus.EventScheduledJob,
		AgentID: "nexus",
		Payload: map[string]any{"job_id": "loopkit:webhook-health"},
	}

	prompt := buildPromptFromEvent(evt, nil)

	if !strings.Contains(prompt, "carries no instruction") {
		t.Errorf("a contentless event should say so, got: %s", prompt)
	}
	if !strings.Contains(prompt, "Do not message the operator") {
		t.Errorf("silence must be an explicitly allowed outcome, got: %s", prompt)
	}
}

// A group message must be rendered as what it is: the right channel, the right
// chat, and a sender who is not assumed to be the operator. The old rendering
// announced every inbound message as "the operator just messaged you on
// WhatsApp", which is how replies ended up aimed at the wrong person.
func TestBuildPromptFromEvent_GroupMessageNamesItsRealOrigin(t *testing.T) {
	evt := bus.Event{
		ID:      "test-evt-group",
		Kind:    bus.EventCommsMessage,
		AgentID: "nexus",
		Payload: map[string]any{
			"content":      "any update on the reports?",
			"channel_id":   "9198@g.us",
			"chat_name":    "CampX Team",
			"channel_type": "slack",
			"sender_id":    "siva",
			"is_group":     true,
		},
	}

	prompt := buildPromptFromEvent(evt, nil)

	if strings.Contains(prompt, "operator just messaged") {
		t.Errorf("a third party's message must not be attributed to the operator, got: %s", prompt)
	}
	if !strings.Contains(prompt, "slack") {
		t.Errorf("the real channel must be named, not a hardcoded one, got: %s", prompt)
	}
	if !strings.Contains(prompt, "CampX Team") || !strings.Contains(prompt, "siva") {
		t.Errorf("chat and sender must both be identified, got: %s", prompt)
	}
	if !strings.Contains(prompt, "GROUP") {
		t.Errorf("a group must be marked as one, got: %s", prompt)
	}
	if !strings.Contains(prompt, "9198@g.us") {
		t.Errorf("the chat id must be present so the reply lands in the right chat, got: %s", prompt)
	}
}
