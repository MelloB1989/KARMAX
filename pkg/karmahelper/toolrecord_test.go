package karmahelper

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/MelloB1989/karma/ai"
	"github.com/MelloB1989/karma/models"
	"github.com/MelloB1989/karmax/internal/tools"
)

type fakeTool struct {
	name string
	res  tools.ToolResult
	err  error
	ran  int
}

func (f *fakeTool) Manifest() tools.ToolManifest {
	return tools.ToolManifest{
		Name:        f.name,
		Description: "a fake tool",
		Parameters:  json.RawMessage(`{"type":"object","properties":{"q":{"type":"string"}}}`),
	}
}

func (f *fakeTool) Execute(ctx context.Context, in map[string]any) (tools.ToolResult, error) {
	f.ran++
	return f.res, f.err
}

// The whole point of the recorder: karma runs the tool and its final response
// says nothing about it, so the wrapper is the only witness.
func TestWrapperRecordsWhatActuallyRan(t *testing.T) {
	rec := &callRecorder{}
	ft := &fakeTool{name: "comms.send", res: tools.SuccessResult(map[string]any{"ok": true})}

	gt := karmaxToolToGoFunctionTool(ft, rec, nil)
	if _, err := gt.Handler(context.Background(), ai.FuncParams{"q": "hello", "__history": "ignored"}); err != nil {
		t.Fatalf("handler: %v", err)
	}

	got := rec.take()
	if len(got) != 1 {
		t.Fatalf("expected 1 recorded call, got %d", len(got))
	}
	if got[0].Name != "comms.send" {
		t.Errorf("name = %q, want the dotted manifest name", got[0].Name)
	}
	if got[0].Input["q"] != "hello" {
		t.Errorf("input not captured: %#v", got[0].Input)
	}
	// __history is karma plumbing, not a tool argument.
	if _, ok := got[0].Input["__history"]; ok {
		t.Error("__history leaked into the recorded input")
	}
	// take() drains, so the next turn starts clean.
	if len(rec.take()) != 0 {
		t.Error("take did not drain the recorder")
	}
}

// A tool that refused still ran. Reporting zero calls there is what made the
// act-evidence guard re-prompt an agent that had genuinely tried.
func TestFailedAndErroringCallsAreStillRecorded(t *testing.T) {
	for _, tc := range []struct {
		name string
		tool *fakeTool
	}{
		{"tool returned an error", &fakeTool{name: "a.tool", err: errors.New("boom")}},
		{"tool reported IsError", &fakeTool{name: "b.tool", res: tools.ToolResult{IsError: true, Error: "refused"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := &callRecorder{}
			gt := karmaxToolToGoFunctionTool(tc.tool, rec, nil)
			_, _ = gt.Handler(context.Background(), ai.FuncParams{})
			if got := rec.take(); len(got) != 1 {
				t.Fatalf("expected the attempt to be recorded, got %d", len(got))
			}
		})
	}
}

// Several tools in one turn are all recorded, in order.
func TestMultipleCallsAccumulate(t *testing.T) {
	rec := &callRecorder{}
	for _, n := range []string{"one", "two", "three"} {
		gt := karmaxToolToGoFunctionTool(&fakeTool{name: n, res: tools.SuccessResult(nil)}, rec, nil)
		if _, err := gt.Handler(context.Background(), ai.FuncParams{}); err != nil {
			t.Fatalf("handler %s: %v", n, err)
		}
	}
	got := rec.take()
	if len(got) != 3 {
		t.Fatalf("got %d calls, want 3", len(got))
	}
	for i, want := range []string{"one", "two", "three"} {
		if got[i].Name != want {
			t.Errorf("call %d = %q, want %q", i, got[i].Name, want)
		}
	}
}

// A nil recorder must not panic — buildKarmaAI is called in paths that predate it.
func TestNilRecorderIsSafe(t *testing.T) {
	gt := karmaxToolToGoFunctionTool(&fakeTool{name: "x", res: tools.SuccessResult(nil)}, nil, nil)
	if _, err := gt.Handler(context.Background(), ai.FuncParams{}); err != nil {
		t.Fatalf("handler: %v", err)
	}
}

// End-to-end for the plumbing the act-evidence guard depends on: what the
// wrapper recorded must reach the caller, even though karma's response carries
// no tool calls of its own.
func TestProcessResponseReportsRecordedCalls(t *testing.T) {
	s := &Session{rec: &callRecorder{}}
	s.rec.add(ToolCallRecord{Name: "comms.send", Input: map[string]any{"target": "x"}})

	// A karma response with an empty ToolCalls slice — the Anthropic path's shape.
	resp := &models.AIChatResponse{AIResponse: "I've sent it.", InputTokens: 10, OutputTokens: 3, Tokens: 13}

	text, records, tokens, err := s.processResponse(resp)
	if err != nil {
		t.Fatalf("processResponse: %v", err)
	}
	if text != "I've sent it." {
		t.Errorf("text = %q", text)
	}
	if len(records) != 1 || records[0].Name != "comms.send" {
		t.Fatalf("the recorded call did not reach the caller: %#v", records)
	}
	if tokens.InputTokens != 10 || tokens.OutputTokens != 3 {
		t.Errorf("tokens = %+v", tokens)
	}
	// Drained, so the next turn does not inherit it.
	if len(s.rec.take()) != 0 {
		t.Error("processResponse left calls in the recorder")
	}
}

// A turn that ran tools but produced no text still ran them; the caller needs
// that fact to distinguish it from a turn that did nothing at all.
func TestToolCallsSurviveAnEmptyResponse(t *testing.T) {
	s := &Session{rec: &callRecorder{}}
	s.rec.add(ToolCallRecord{Name: "memory.ingest"})

	_, records, _, err := s.processResponse(&models.AIChatResponse{AIResponse: "   "})
	if err == nil {
		t.Fatal("an empty response should still be an error")
	}
	if len(records) != 1 {
		t.Errorf("tool calls were dropped on the empty-response path: %#v", records)
	}
}
