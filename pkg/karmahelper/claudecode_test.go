package karmahelper

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/MelloB1989/karmax/internal/tools"
)

type ccFakeTool struct {
	name  string
	calls int
	mu    sync.Mutex
}

func (f *ccFakeTool) Manifest() tools.ToolManifest {
	return tools.ToolManifest{Name: f.name, Description: "does " + f.name,
		Parameters: json.RawMessage(`{"type":"object","properties":{"q":{"type":"string"}},"required":["q"]}`)}
}
func (f *ccFakeTool) Execute(_ context.Context, in map[string]any) (tools.ToolResult, error) {
	f.mu.Lock()
	f.calls++
	f.mu.Unlock()
	return tools.ToolResult{Output: "result for " + f.name}, nil
}

var turnFlag = regexp.MustCompile(`--turn ([0-9a-f]{32})`)

// installRunner swaps the runner for the test and restores it after.
func installRunner(t *testing.T, fn ClaudeCodeRunner) {
	t.Helper()
	prev := claudeCodeRunner()
	SetClaudeCodeRunner(fn)
	t.Cleanup(func() { SetClaudeCodeRunner(prev) })
}

func ccSession(key, system string, ts ...tools.Tool) *Session {
	return NewSession(SessionConfig{Provider: ProviderClaudeCode, Kind: "memory", AgentID: "a",
		SystemPrompt: system, SessionKey: key}, ts)
}

// A claude-code session has no API client at all: an unknown provider name
// resolves to real OpenAI, and nothing about this path may reach it.
func TestClaudeCodeSessionHasNoAPIClient(t *testing.T) {
	if s := ccSession("t/none", "x"); s.kai != nil {
		t.Fatal("a claude-code session built an API client")
	}
}

func TestNoRunnerIsAnErrorNotAFallthrough(t *testing.T) {
	installRunner(t, nil)
	_, _, _, err := ccSession("t/norunner", "x").Chat(context.Background(), "hi")
	if err == nil || !strings.Contains(err.Error(), "not available") {
		t.Fatalf("err = %v, want a plain not-available error", err)
	}
}

func TestInstructionsGoOncePerKeyAndContextEveryTurn(t *testing.T) {
	var prompts []string
	installRunner(t, func(_ context.Context, turn ClaudeCodeTurn) (string, error) {
		prompts = append(prompts, turn.Prompt)
		if turn.Kind != "memory" || turn.Key != "api/t/prime-once" {
			t.Errorf("turn = %+v", turn)
		}
		return "answer", nil
	})
	s := ccSession("t/prime-once", "YOU ARE THE RETRIEVER")
	s.SetContext("today is Tuesday")
	for i := 0; i < 2; i++ {
		out, _, _, err := s.Chat(context.Background(), "question")
		if err != nil || out != "answer" {
			t.Fatalf("turn %d: %q %v", i, out, err)
		}
	}
	if !strings.Contains(prompts[0], "YOU ARE THE RETRIEVER") {
		t.Error("the first turn did not carry the instructions")
	}
	if strings.Contains(prompts[1], "YOU ARE THE RETRIEVER") {
		t.Error("a warm session was sent its instructions again")
	}
	for i, p := range prompts {
		if !strings.Contains(p, "today is Tuesday") || !strings.HasSuffix(p, "question") {
			t.Errorf("turn %d prompt lost its context or message: %q", i, p)
		}
	}
	if n := len(s.GetHistory().Messages); n != 4 {
		t.Errorf("history has %d messages, want 4", n)
	}
}

// The mechanism the memory sub-agent depends on: its tools exist only inside
// the session, and the model reaches them through the turn token.
func TestTurnToolsAreReachableAndRecorded(t *testing.T) {
	search := &ccFakeTool{name: "mem_search"}
	var token string
	installRunner(t, func(ctx context.Context, turn ClaudeCodeTurn) (string, error) {
		m := turnFlag.FindStringSubmatch(turn.Prompt)
		if m == nil {
			t.Fatal("the prompt carries no --turn token")
		}
		token = m[1]
		if !strings.Contains(turn.Prompt, "mem_search") || !strings.Contains(turn.Prompt, "q (string, required)") {
			t.Errorf("the tool is not described usefully: %q", turn.Prompt)
		}
		if _, err := CallTurnTool(ctx, token, "mem_search", map[string]any{"q": "cab"}); err != nil {
			t.Fatalf("calling a granted tool: %v", err)
		}
		return "found it", nil
	})
	out, records, _, err := ccSession("t/tools", "x", search).Chat(context.Background(), "find the cab costs")
	if err != nil || out != "found it" {
		t.Fatalf("%q %v", out, err)
	}
	if search.calls != 1 {
		t.Errorf("tool ran %d times", search.calls)
	}
	if len(records) != 1 || records[0].Name != "mem_search" {
		t.Errorf("records = %+v, want the call the model made", records)
	}
	// The grant dies with the turn.
	if _, err := CallTurnTool(context.Background(), token, "mem_search", nil); !errors.Is(err, ErrUnknownTurn) {
		t.Errorf("after the turn: err = %v, want ErrUnknownTurn", err)
	}
}

// A withheld tool is not in the grant, so the token cannot reach it.
func TestWithheldToolsAreNotGranted(t *testing.T) {
	send, read := &ccFakeTool{name: "comms.send"}, &ccFakeTool{name: "mem_recent"}
	installRunner(t, func(ctx context.Context, turn ClaudeCodeTurn) (string, error) {
		token := turnFlag.FindStringSubmatch(turn.Prompt)[1]
		if strings.Contains(turn.Prompt, "comms.send") {
			t.Error("a withheld tool was advertised")
		}
		for _, spelling := range []string{"comms.send", "comms_send"} {
			if _, err := CallTurnTool(ctx, token, spelling, map[string]any{}); !errors.Is(err, ErrNotInTurn) {
				t.Errorf("withheld tool as %q: err = %v, want ErrNotInTurn", spelling, err)
			}
		}
		if _, err := CallTurnTool(ctx, token, "mem_recent", map[string]any{}); err != nil {
			t.Errorf("a granted tool was refused: %v", err)
		}
		return "observed", nil
	})
	s := ccSession("t/withhold", "x", send, read)
	// Canonical, as every real caller passes it.
	if _, _, _, err := s.ChatWithTurnTools(context.Background(), "watch", nil, map[string]bool{tools.CanonicalName("comms.send"): true}); err != nil {
		t.Fatal(err)
	}
	if send.calls != 0 {
		t.Fatal("a withheld tool ran")
	}
}

func TestPrimeTurnLeavesHistoryAlone(t *testing.T) {
	var got string
	installRunner(t, func(_ context.Context, turn ClaudeCodeTurn) (string, error) {
		got = turn.Prompt
		return "ok", nil
	})
	s := ccSession("", "CALL INSTRUCTIONS")
	s.SetContext("brief about the caller")
	prime := s.PrimeTurn("a call has connected")
	if prime == nil {
		t.Fatal("no prime for a claude-code session")
	}
	if err := prime(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "CALL INSTRUCTIONS") || !strings.Contains(got, "brief about the caller") {
		t.Errorf("prime prompt = %q", got)
	}
	if n := len(s.GetHistory().Messages); n != 0 {
		t.Errorf("priming added %d messages to history", n)
	}
	// The instructions went with the prime, so the first real turn is warm.
	var first string
	installRunner(t, func(_ context.Context, turn ClaudeCodeTurn) (string, error) { first = turn.Prompt; return "hi", nil })
	_, _, _, _ = s.Chat(context.Background(), "hello")
	if strings.Contains(first, "CALL INSTRUCTIONS") {
		t.Error("the first real turn repeated what the prime already sent")
	}
}

func TestPrimeTurnIsNilForAPISessions(t *testing.T) {
	s := NewSession(SessionConfig{Provider: "anthropic", Model: "x"}, nil)
	if s.PrimeTurn("x") != nil {
		t.Fatal("an API session was primed")
	}
}

func TestUnkeyedSessionsDoNotShareAConversation(t *testing.T) {
	a, b := ccSession("", "x"), ccSession("", "x")
	if a.sessionKey() == b.sessionKey() {
		t.Fatal("two unkeyed sessions share a Claude Code conversation")
	}
	if a.sessionKey() != a.sessionKey() {
		t.Fatal("a session's key is not stable")
	}
}
