package harness

import (
	"bufio"
	"encoding/json"
	"os"
	"testing"
)

// collect replays a list of raw CLI lines and returns what the sink saw.
func collect(t *testing.T, lines ...string) []Event {
	t.Helper()
	var evs []event
	for _, l := range lines {
		var ev event
		if err := json.Unmarshal([]byte(l), &ev); err != nil {
			t.Fatalf("fixture line is not json: %v\n%s", err, l)
		}
		evs = append(evs, ev)
	}
	var got []Event
	replay(func(e Event) { got = append(got, e) }, evs)
	return got
}

func TestTextDeltasBecomeMessages(t *testing.T) {
	got := collect(t,
		`{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"Found "}}}`,
		`{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"14."}}}`,
	)
	if len(got) != 2 || got[0].Kind != KindMessage || got[1].Text != "14." {
		t.Fatalf("got %+v", got)
	}
}

// Reasoning is a different stream from the reply, and the delta field it
// arrives in is named "thinking", not "text".
func TestThinkingDeltasBecomeThoughts(t *testing.T) {
	got := collect(t,
		`{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"thinking_delta","thinking":"Let me check "}}}`,
	)
	if len(got) != 1 || got[0].Kind != KindThought || got[0].Text != "Let me check " {
		t.Fatalf("got %+v", got)
	}
}

// The whole point of the id: two calls to one tool are two calls.
func TestAToolCallIsAnnouncedWithItsIdentity(t *testing.T) {
	got := collect(t,
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"toolu_1","name":"Read","input":{"file_path":"/a/main.go"}}]}}`,
	)
	if len(got) != 1 {
		t.Fatalf("got %d events, want 1: %+v", len(got), got)
	}
	e := got[0]
	if e.Kind != KindTool || e.Tool == nil {
		t.Fatalf("got %+v", e)
	}
	if e.Tool.ID != "toolu_1" || e.Tool.Title != "main.go" || e.Tool.Kind != ToolRead {
		t.Errorf("identity wrong: %+v", e.Tool)
	}
	if e.Tool.Status != StatusInProgress {
		t.Errorf("status = %q, want in_progress", e.Tool.Status)
	}
	if len(e.Tool.Locations) != 1 || e.Tool.Locations[0].Path != "/a/main.go" {
		t.Errorf("locations wrong: %+v", e.Tool.Locations)
	}
}

func TestAToolResultResolvesByID(t *testing.T) {
	got := collect(t,
		`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"package main"}]}}`,
	)
	if len(got) != 1 || got[0].Kind != KindToolUpdate || got[0].Tool == nil {
		t.Fatalf("got %+v", got)
	}
	if got[0].Tool.ID != "toolu_1" || got[0].Tool.Status != StatusCompleted {
		t.Errorf("got %+v", got[0].Tool)
	}
	if got[0].Tool.Output != "package main" {
		t.Errorf("output = %q", got[0].Tool.Output)
	}
}

func TestAFailedToolResultSaysSo(t *testing.T) {
	got := collect(t,
		`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"toolu_2","is_error":true,"content":"no such file"}]}}`,
	)
	if len(got) != 1 || got[0].Tool.Status != StatusFailed {
		t.Fatalf("got %+v", got)
	}
}

// TodoWrite is a plan, not a tool call worth a line of its own.
func TestTodoWriteBecomesAPlanAndNotATool(t *testing.T) {
	got := collect(t,
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"toolu_3","name":"TodoWrite","input":{"todos":[{"content":"Ship it","status":"pending"}]}}]}}`,
	)
	if len(got) != 1 || got[0].Kind != KindPlan {
		t.Fatalf("got %+v", got)
	}
	if len(got[0].Plan) != 1 || got[0].Plan[0].Content != "Ship it" {
		t.Errorf("plan wrong: %+v", got[0].Plan)
	}
}

// The bug this file was written for: --include-partial-messages sends the
// deltas AND the finished text again, so emitting both doubles every reply.
func TestAssistantTextIsNotEmittedTwice(t *testing.T) {
	got := collect(t,
		`{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"Hello"}}}`,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"Hello"}]}}`,
	)
	if len(got) != 1 {
		t.Fatalf("got %d events, want 1 — the reply was doubled: %+v", len(got), got)
	}
}

func TestANilSinkIsNotACrash(t *testing.T) {
	replay(nil, []event{{Type: "assistant"}})
}

// Ground truth from a real turn with thinking enabled: reasoning and reply
// must not leak into each other.
func TestEmptyTextDeltaEmitsNothing(t *testing.T) {
	got := collect(t,
		`{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":""}}}`,
	)
	if len(got) != 0 {
		t.Fatalf("got %d events, want 0 (empty text_delta should not emit): %+v", len(got), got)
	}
}

func TestEmptyThinkingDeltaEmitsNothing(t *testing.T) {
	got := collect(t,
		`{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"thinking_delta","thinking":""}}}`,
	)
	if len(got) != 0 {
		t.Fatalf("got %d events, want 0 (empty thinking_delta should not emit): %+v", len(got), got)
	}
}

func TestRecordedThinkingTurnSeparatesThoughtFromReply(t *testing.T) {
	f, err := os.Open("testdata/thinking.jsonl")
	if err != nil {
		t.Skip("no thinking fixture recorded yet")
	}
	defer f.Close()

	var thought, message int
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for sc.Scan() {
		var ev event
		if json.Unmarshal(sc.Bytes(), &ev) != nil {
			continue
		}
		emit(func(e Event) {
			switch e.Kind {
			case KindThought:
				thought++
				if e.Text == "" {
					t.Error("a thought arrived empty — wrong delta field")
				}
			case KindMessage:
				message++
			}
		}, ev)
	}
	if thought == 0 {
		t.Error("no thoughts emitted from a turn recorded with thinking on")
	}
	if message == 0 {
		t.Error("no reply emitted")
	}
}
