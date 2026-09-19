package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/MelloB1989/karmax/internal/config"
	"go.uber.org/zap"
)

// The wire carries two events the harness never emits, and they bracket the
// ones it does.
func TestStreamBracketsHarnessEventsWithConversationAndDone(t *testing.T) {
	var lines []string
	w := &lineSink{onLine: func(s string) { lines = append(lines, s) }}

	streamTurn(w, "sess-1", true, func(sink func(harnessEvent)) (string, error) {
		sink(harnessEvent{Kind: "message", Text: "Found "})
		sink(harnessEvent{Kind: "tool", Tool: &ChatTool{ID: "t1", Status: "in_progress"}})
		sink(harnessEvent{Kind: "message", Text: "14."})
		return "Found 14.", nil
	})

	kinds := kindsOf(t, lines)
	want := []string{"conversation", "message", "tool", "message", "done"}
	if strings.Join(kinds, ",") != strings.Join(want, ",") {
		t.Fatalf("kinds = %v, want %v", kinds, want)
	}
}

// An existing conversation is not re-announced; the client already has the id.
func TestExistingConversationIsNotAnnounced(t *testing.T) {
	var lines []string
	w := &lineSink{onLine: func(s string) { lines = append(lines, s) }}
	streamTurn(w, "sess-1", false, func(sink func(harnessEvent)) (string, error) {
		sink(harnessEvent{Kind: "message", Text: "hi"})
		return "hi", nil
	})
	if got := kindsOf(t, lines); got[0] != "message" {
		t.Fatalf("first event = %q, want message", got[0])
	}
}

// A failure after partial output still ends the stream with something the
// client can act on. Headers left long ago; a status code is not available.
func TestFailureEndsTheStream(t *testing.T) {
	var lines []string
	w := &lineSink{onLine: func(s string) { lines = append(lines, s) }}
	streamTurn(w, "sess-1", false, func(sink func(harnessEvent)) (string, error) {
		sink(harnessEvent{Kind: "message", Text: "partial"})
		return "", errTest
	})
	kinds := kindsOf(t, lines)
	if kinds[len(kinds)-1] != "error" {
		t.Fatalf("last event = %q, want error", kinds[len(kinds)-1])
	}
}

// A background delegation becomes a ticket event, before done.
func TestBackgroundWorkBecomesATicket(t *testing.T) {
	var lines []string
	w := &lineSink{onLine: func(s string) { lines = append(lines, s) }}
	streamTurn(w, "s", false, func(sink func(harnessEvent)) (string, error) {
		sink(harnessEvent{Kind: "message", Text: "That will take a while."})
		sink(harnessEvent{Kind: "ticket", JobID: "job-1", Text: "Refactor the auth module"})
		return "That will take a while.", nil
	})
	kinds := kindsOf(t, lines)
	if strings.Join(kinds, ",") != "message,ticket,done" {
		t.Fatalf("kinds = %v", kinds)
	}
}

// Each kind writes only its own fields. A blanket dump would put an empty
// "tool" on every text delta and force the client's union to make every field
// optional to read it.
func TestStreamTurnWritesOneShapePerKind(t *testing.T) {
	var lines []string
	sink := &lineSink{onLine: func(s string) { lines = append(lines, s) }}

	streamTurn(sink, "conv-1", true, func(emit func(harnessEvent)) (string, error) {
		emit(harnessEvent{Kind: "message", Text: "Found "})
		emit(harnessEvent{Kind: "thought", Text: "checking"})
		emit(harnessEvent{Kind: "tool", Tool: &ChatTool{
			ID: "toolu_1", Title: "main.go", Kind: "read", Status: "in_progress",
			Locations: []ChatLocation{{Path: "/a/main.go", Line: 12}},
		}})
		emit(harnessEvent{Kind: "tool_update", Tool: &ChatTool{
			ID: "toolu_1", Status: "completed", Output: "package main",
		}})
		emit(harnessEvent{Kind: "plan", Plan: []ChatPlanEntry{{Content: "Ship it", Status: "pending"}}})
		// The footer's facts, so the meta case is actually exercised here and
		// not only by the runtime adapter's own (untested-by-this-package) call.
		emit(harnessEvent{Kind: "meta", Model: "claude-x", DurationMS: 1234, CostUSD: 0.05})
		return "Found 14.", nil
	})

	got := make([]map[string]any, 0, len(lines))
	for _, l := range lines {
		var m map[string]any
		if err := json.Unmarshal([]byte(l), &m); err != nil {
			t.Fatalf("line is not json: %s", l)
		}
		got = append(got, m)
	}

	if got[0]["kind"] != "conversation" || got[0]["id"] != "conv-1" {
		t.Fatalf("first line: %+v", got[0])
	}
	if got[1]["kind"] != "message" || got[1]["text"] != "Found " {
		t.Errorf("message: %+v", got[1])
	}
	if _, extra := got[1]["tool"]; extra {
		t.Error("a message line carried a tool field")
	}
	if got[2]["kind"] != "thought" || got[2]["text"] != "checking" {
		t.Errorf("thought: %+v", got[2])
	}

	tool, ok := got[3]["tool"].(map[string]any)
	if !ok {
		t.Fatalf("tool line has no tool object: %+v", got[3])
	}
	if tool["id"] != "toolu_1" || tool["title"] != "main.go" || tool["kind"] != "read" {
		t.Errorf("tool: %+v", tool)
	}
	locs, ok := tool["locations"].([]any)
	if !ok || len(locs) != 1 {
		t.Errorf("locations: %+v", tool["locations"])
	}

	upd := got[4]["tool"].(map[string]any)
	if upd["id"] != "toolu_1" || upd["status"] != "completed" {
		t.Errorf("update: %+v", upd)
	}

	plan, ok := got[5]["plan"].([]any)
	if !ok || len(plan) != 1 {
		t.Fatalf("plan: %+v", got[5])
	}

	meta := got[len(got)-2]
	if meta["kind"] != "meta" {
		t.Fatalf("meta: %+v", meta)
	}
	if _, ok := meta["model"]; !ok {
		t.Errorf("meta line missing model field: %+v", meta)
	}

	last := got[len(got)-1]
	if last["kind"] != "done" || last["text"] != "Found 14." {
		t.Errorf("done: %+v", last)
	}
}

// The error goes down the stream, not into a status code: the headers left
// before the first token did.
func TestStreamTurnReportsFailureInBand(t *testing.T) {
	var lines []string
	sink := &lineSink{onLine: func(s string) { lines = append(lines, s) }}
	streamTurn(sink, "conv-2", false, func(emit func(harnessEvent)) (string, error) {
		emit(harnessEvent{Kind: "message", Text: "part"})
		return "", errTest
	})
	var last map[string]any
	_ = json.Unmarshal([]byte(lines[len(lines)-1]), &last)
	if last["kind"] != "error" {
		t.Fatalf("got %+v", last)
	}
}

func newChatOptionsTestServer(t *testing.T, cfg *config.KarmaxConfig) *Server {
	t.Helper()
	return New("127.0.0.1:0", 0, "", "", nil, nil, nil, nil, cfg, zap.NewNop())
}

func postChatStream(srv *Server, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, "/api/chat/stream", bytes.NewReader([]byte(body)))
	w := httptest.NewRecorder()
	srv.handleChatStream(w, r)
	return w
}

// An effort outside the CLI's own vocabulary must not reach spawnArgs, where
// it would just make the CLI itself reject the process.
func TestHandleChatStreamRejectsInvalidEffort(t *testing.T) {
	srv := newChatOptionsTestServer(t, &config.KarmaxConfig{})
	w := postChatStream(srv, `{"message":"hi","effort":"turbo"}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", w.Code, w.Body.String())
	}
}

// A model value outside the allowed charset is refused before it ever
// reaches a shell-adjacent --model flag.
func TestHandleChatStreamRejectsInvalidModel(t *testing.T) {
	srv := newChatOptionsTestServer(t, &config.KarmaxConfig{})
	w := postChatStream(srv, `{"message":"hi","model":"not a model!"}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", w.Code, w.Body.String())
	}
}

// A well-formed model/effort pair passes validation and reaches the "brain
// not running" branch — proving it got past the new checks rather than
// failing on them.
func TestHandleChatStreamAcceptsValidModelAndEffort(t *testing.T) {
	srv := newChatOptionsTestServer(t, &config.KarmaxConfig{})
	w := postChatStream(srv, `{"message":"hi","model":"opus","effort":"high"}`)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 (no chatTurn wired): %s", w.Code, w.Body.String())
	}
}

// Empty model/effort mean "use the defaults" — they must never be rejected.
func TestHandleChatStreamAllowsEmptyModelAndEffort(t *testing.T) {
	srv := newChatOptionsTestServer(t, &config.KarmaxConfig{})
	w := postChatStream(srv, `{"message":"hi"}`)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 (no chatTurn wired): %s", w.Code, w.Body.String())
	}
}

// The Claude harness gets the real pickers, and the chat kind's own
// configured model is what an empty request actually runs on.
func TestHandleChatOptionsForClaudeBrain(t *testing.T) {
	cfg := &config.KarmaxConfig{Harness: config.HarnessConfig{
		Kinds: map[string]config.HarnessKindConfig{"chat": {Model: "sonnet"}},
	}}
	srv := newChatOptionsTestServer(t, cfg)
	r := httptest.NewRequest(http.MethodGet, "/api/chat/options", nil)
	w := httptest.NewRecorder()
	srv.handleChatOptions(w, r)

	var got map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("response is not json: %v", err)
	}
	if got["brain"] != "claude" {
		t.Errorf("brain = %v, want claude", got["brain"])
	}
	models, ok := got["models"].([]any)
	if !ok || len(models) != 4 {
		t.Fatalf("models = %+v, want the 4 aliases", got["models"])
	}
	efforts, ok := got["efforts"].([]any)
	if !ok || len(efforts) != 5 {
		t.Fatalf("efforts = %+v, want the 5 levels", got["efforts"])
	}
	if got["defaultModel"] != "sonnet" {
		t.Errorf("defaultModel = %v, want the configured chat kind's model", got["defaultModel"])
	}
	if got["defaultEffort"] != "" {
		t.Errorf("defaultEffort = %v, want empty", got["defaultEffort"])
	}
}

// A non-Claude brain (codex) offers no pickers at all — empty arrays, never
// null, so a client can hide them without a nil check.
func TestHandleChatOptionsForNonClaudeBrainIsEmptyNotNull(t *testing.T) {
	cfg := &config.KarmaxConfig{Harness: config.HarnessConfig{Binary: "codex"}}
	srv := newChatOptionsTestServer(t, cfg)
	r := httptest.NewRequest(http.MethodGet, "/api/chat/options", nil)
	w := httptest.NewRecorder()
	srv.handleChatOptions(w, r)

	body := w.Body.String()
	if !strings.Contains(body, `"models":[]`) {
		t.Errorf(`models must be "[]", not null, for a non-Claude brain: %s`, body)
	}
	if !strings.Contains(body, `"efforts":[]`) {
		t.Errorf(`efforts must be "[]", not null, for a non-Claude brain: %s`, body)
	}
}

func kindsOf(t *testing.T, lines []string) []string {
	t.Helper()
	var out []string
	for _, l := range lines {
		if strings.TrimSpace(l) == "" {
			continue
		}
		var e struct {
			Kind string `json:"kind"`
		}
		if err := json.Unmarshal([]byte(l), &e); err != nil {
			t.Fatalf("line is not JSON: %q", l)
		}
		out = append(out, e.Kind)
	}
	return out
}
