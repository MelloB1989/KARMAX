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
	if !strings.Contains(prompt, "comms.message") {
		t.Error("prompt should contain event kind 'comms.message'")
	}
	if !strings.Contains(prompt, "Hello KARMAX") {
		t.Error("prompt should contain the message content from payload")
	}
	if !strings.Contains(prompt, "discord-main") {
		t.Error("prompt should contain karmax_channel_id")
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
