package agent

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/MelloB1989/karmax/internal/tools"
	"github.com/MelloB1989/karmax/pkg/karmahelper"
)

func TestEncodeToolCallsRecordsNameAndInput(t *testing.T) {
	got := encodeToolCalls([]karmahelper.ToolCallRecord{
		{Name: "comms.send", Input: map[string]any{"text": "on it"}},
		{Name: "self.remind", Input: map[string]any{"in": "2h"}},
	})

	var decoded []map[string]any
	if err := json.Unmarshal([]byte(got), &decoded); err != nil {
		t.Fatalf("stored tool calls must be valid JSON: %v (%s)", err, got)
	}
	if len(decoded) != 2 {
		t.Fatalf("expected 2 calls, got %d: %s", len(decoded), got)
	}
	if decoded[0]["name"] != "comms.send" {
		t.Errorf("first call name = %v", decoded[0]["name"])
	}
}

func TestEncodeToolCallsKeepsFailures(t *testing.T) {
	got := encodeToolCalls([]karmahelper.ToolCallRecord{
		{Name: "whatsapp.read", Error: errors.New("wacli unreachable")},
		{Name: "google", Result: tools.ToolResult{IsError: true, Error: "token expired"}},
	})
	// A turn that tried and failed is exactly what is worth reading back.
	for _, want := range []string{"wacli unreachable", "token expired"} {
		if !strings.Contains(got, want) {
			t.Errorf("expected %q in %s", want, got)
		}
	}
}

func TestEncodeToolCallsEmptyStaysEmpty(t *testing.T) {
	if got := encodeToolCalls(nil); got != "" {
		t.Errorf("a turn with no tool calls should store nothing, got %q", got)
	}
}

func TestTruncateToolInputBoundsLongValuesButKeepsKeys(t *testing.T) {
	long := strings.Repeat("x", maxStoredToolInput*2)
	out := truncateToolInput(map[string]any{"chat": "Dev", "text": long, "limit": 50})

	if out["chat"] != "Dev" {
		t.Errorf("short values must survive intact, got %v", out["chat"])
	}
	if out["limit"] != 50 {
		t.Errorf("non-strings must pass through, got %v", out["limit"])
	}
	got, _ := out["text"].(string)
	if len([]rune(got)) > maxStoredToolInput+1 {
		t.Errorf("long value not truncated: %d runes", len([]rune(got)))
	}
	if !strings.HasSuffix(got, "…") {
		t.Errorf("truncation should be visible, got tail %q", got[len(got)-10:])
	}
}
