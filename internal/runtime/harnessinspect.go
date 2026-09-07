package runtime

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/MelloB1989/karmax/internal/tools"
)

// Looking inside a session, and managing it.
//
// A session is a process, a transcript and a row, and until now only the row
// was visible. That is enough to know a conversation exists and nothing about
// what it did — which is the wrong side of the line for something running with
// a real shell on the operator's machine.

type harnessShowTool struct{ ref *harnessRef }

func (t *harnessShowTool) Manifest() tools.ToolManifest {
	return tools.ToolManifest{
		Name: "harness.show",
		Description: "Everything about one Claude Code session: its model, state, how many turns " +
			"it has taken, what it has cost, where it is running, and how long since it last did " +
			"anything.",
		Parameters: json.RawMessage(`{
			"type":"object",
			"properties":{"key":{"type":"string","description":"The session key, from harness.list."}},
			"required":["key"]
		}`),
	}
}

func (t *harnessShowTool) Execute(_ context.Context, in map[string]any) (tools.ToolResult, error) {
	rt := t.ref.get()
	if rt == nil {
		return tools.ErrorResult(fmt.Errorf("the runtime is not ready")), nil
	}
	key, _ := in["key"].(string)
	rec, err := rt.store.GetHarnessSession(strings.TrimSpace(key))
	if err != nil {
		return tools.ErrorResult(err), nil
	}
	if rec == nil {
		return tools.ErrorResult(fmt.Errorf("no session %q", key)), nil
	}
	live := false
	if rt.harness != nil {
		for _, k := range rt.harness.Live() {
			if k == rec.Key {
				live = true
			}
		}
	}
	out := map[string]any{
		"key": rec.Key, "kind": rec.Kind, "model": rec.Model,
		"state": rec.State, "live": live, "pid": rec.PID,
		"turns": rec.Turns, "cost_usd": rec.CostUSD,
		"workdir": rec.Workdir, "transcript_id": rec.HarnessSessionID,
		"started":  rec.StartedAt.Format(time.RFC3339),
		"idle_for": time.Since(rec.LastActivityAt).Round(time.Second).String(),
		"tokens": map[string]any{
			"input": rec.InputTokens, "output": rec.OutputTokens, "cache_read": rec.CacheRead,
		},
	}
	if rec.LastError != "" {
		out["last_error"] = rec.LastError
	}
	if p := transcriptPath(rec.Workdir, rec.HarnessSessionID); p != "" {
		if st, err := os.Stat(p); err == nil {
			out["transcript"] = map[string]any{"path": p, "bytes": st.Size()}
		}
	}
	return tools.SuccessResult(out), nil
}

type harnessModelTool struct{ ref *harnessRef }

func (t *harnessModelTool) Manifest() tools.ToolManifest {
	return tools.ToolManifest{
		Name: "harness.model",
		Description: "Move a session to a different model — haiku, sonnet, opus, fable. " +
			"A model is chosen when the process starts, so this closes the running one; the " +
			"next message resumes the same conversation on the new tier with its context intact.",
		Parameters: json.RawMessage(`{
			"type":"object",
			"properties":{
				"key":{"type":"string","description":"The session key."},
				"model":{"type":"string","description":"haiku | sonnet | opus | fable, or a full model id."}
			},
			"required":["key","model"]
		}`),
	}
}

func (t *harnessModelTool) Execute(_ context.Context, in map[string]any) (tools.ToolResult, error) {
	rt := t.ref.get()
	if rt == nil || rt.harness == nil {
		return tools.ErrorResult(fmt.Errorf("the harness is not enabled")), nil
	}
	key, _ := in["key"].(string)
	model, _ := in["model"].(string)
	key, model = strings.TrimSpace(key), strings.TrimSpace(model)
	if key == "" || model == "" {
		return tools.ErrorResult(fmt.Errorf("key and model are both required")), nil
	}
	rec, err := rt.store.GetHarnessSession(key)
	if err != nil {
		return tools.ErrorResult(err), nil
	}
	if rec == nil {
		return tools.ErrorResult(fmt.Errorf("no session %q", key)), nil
	}
	was := rec.Model
	if err := rt.store.SetHarnessModel(key, model); err != nil {
		return tools.ErrorResult(err), nil
	}
	// Closed rather than left running, because the process already has its
	// model. The row keeps the transcript id, so the next message resumes this
	// conversation rather than starting a new one.
	rt.harness.Close(key)
	return tools.SuccessResult(map[string]any{
		"key": key, "was": was, "now": model,
		"note": "the running process was closed; the next message resumes this conversation on the new model",
	}), nil
}

type harnessTranscriptTool struct{ ref *harnessRef }

func (t *harnessTranscriptTool) Manifest() tools.ToolManifest {
	return tools.ToolManifest{
		Name: "harness.transcript",
		Description: "Read what a session actually said and did — the last N exchanges, including " +
			"which tools it ran. This is how to find out what a long-running task has been doing, " +
			"rather than inferring it from its status.",
		Parameters: json.RawMessage(`{
			"type":"object",
			"properties":{
				"key":{"type":"string","description":"The session key."},
				"tail":{"type":"integer","description":"How many recent entries to return. Default 20."}
			},
			"required":["key"]
		}`),
	}
}

func (t *harnessTranscriptTool) Execute(_ context.Context, in map[string]any) (tools.ToolResult, error) {
	rt := t.ref.get()
	if rt == nil {
		return tools.ErrorResult(fmt.Errorf("the runtime is not ready")), nil
	}
	key, _ := in["key"].(string)
	rec, err := rt.store.GetHarnessSession(strings.TrimSpace(key))
	if err != nil {
		return tools.ErrorResult(err), nil
	}
	if rec == nil {
		return tools.ErrorResult(fmt.Errorf("no session %q", key)), nil
	}
	n := 20
	if v, ok := in["tail"].(float64); ok && v > 0 {
		n = int(v)
	}
	path := transcriptPath(rec.Workdir, rec.HarnessSessionID)
	if path == "" {
		return tools.ErrorResult(fmt.Errorf("this session has no transcript on disk yet")), nil
	}
	entries, err := readTranscript(path, n)
	if err != nil {
		return tools.ErrorResult(err), nil
	}
	return tools.SuccessResult(map[string]any{
		"key": rec.Key, "model": rec.Model, "entries": entries, "count": len(entries),
	}), nil
}

type harnessPruneTool struct{ ref *harnessRef }

func (t *harnessPruneTool) Manifest() tools.ToolManifest {
	return tools.ToolManifest{
		Name: "harness.prune",
		Description: "Forget sessions that have already ended. Only closed and dead ones — a row " +
			"is what makes a transcript reachable, so a live or idle session is never dropped.",
		Parameters: json.RawMessage(`{
			"type":"object",
			"properties":{"older_than_hours":{"type":"number","description":"Only sessions idle this long. Default 168 (a week)."}}
		}`),
	}
}

func (t *harnessPruneTool) Execute(_ context.Context, in map[string]any) (tools.ToolResult, error) {
	rt := t.ref.get()
	if rt == nil {
		return tools.ErrorResult(fmt.Errorf("the runtime is not ready")), nil
	}
	hours := 168.0
	if v, ok := in["older_than_hours"].(float64); ok && v > 0 {
		hours = v
	}
	n, err := rt.store.PruneHarnessSessions(time.Now().Add(-time.Duration(hours) * time.Hour))
	if err != nil {
		return tools.ErrorResult(err), nil
	}
	return tools.SuccessResult(map[string]any{"forgotten": n}), nil
}

// transcriptPath locates a session's transcript.
//
// The CLI stores one file per session under a directory named after the working
// directory, with every separator flattened to a dash — so
// /home/n/.karmax/sessions/agent_nexus becomes
// -home-n--karmax-sessions-agent-nexus. Derived rather than recorded because it
// is the CLI's layout and not ours to fix in place if it changes.
func transcriptPath(workdir, sessionID string) string {
	if strings.TrimSpace(workdir) == "" || strings.TrimSpace(sessionID) == "" {
		return ""
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	flat := strings.Map(func(r rune) rune {
		switch r {
		case '/', '.', '_':
			return '-'
		}
		return r
	}, workdir)
	p := filepath.Join(home, ".claude", "projects", flat, sessionID+".jsonl")
	if _, err := os.Stat(p); err != nil {
		return ""
	}
	return p
}

// transcriptEntry is one exchange, reduced to what a person reading it needs.
type transcriptEntry struct {
	Role  string   `json:"role"`
	Text  string   `json:"text,omitempty"`
	Tools []string `json:"tools,omitempty"`
	At    string   `json:"at,omitempty"`
}

// readTranscript returns the last n meaningful entries.
//
// Streamed rather than read whole: a busy session's transcript reaches several
// megabytes, and loading all of it to show twenty lines is how a status command
// becomes something you avoid running.
func readTranscript(path string, n int) ([]transcriptEntry, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)

	// A ring, so memory is bounded by what was asked for rather than by the
	// size of the file.
	ring := make([]transcriptEntry, 0, n)
	for sc.Scan() {
		var line struct {
			Type      string `json:"type"`
			Timestamp string `json:"timestamp"`
			Message   struct {
				Role    string          `json:"role"`
				Content json.RawMessage `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal(sc.Bytes(), &line) != nil {
			continue
		}
		if line.Type != "user" && line.Type != "assistant" {
			continue
		}
		e := transcriptEntry{Role: line.Message.Role, At: line.Timestamp}
		if e.Role == "" {
			e.Role = line.Type
		}
		e.Text, e.Tools = summariseContent(line.Message.Content)
		if e.Text == "" && len(e.Tools) == 0 {
			continue
		}
		if len(ring) == n {
			ring = ring[1:]
		}
		ring = append(ring, e)
	}
	return ring, sc.Err()
}

// summariseContent flattens a content array to text plus the tools it invoked.
func summariseContent(raw json.RawMessage) (string, []string) {
	if len(raw) == 0 {
		return "", nil
	}
	// Content is either a bare string or an array of blocks.
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return trimTo(s, 600), nil
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
		Name string `json:"name"`
	}
	if json.Unmarshal(raw, &blocks) != nil {
		return "", nil
	}
	var sb strings.Builder
	var used []string
	for _, b := range blocks {
		switch b.Type {
		case "text":
			sb.WriteString(b.Text)
		case "tool_use":
			used = append(used, b.Name)
		}
	}
	return trimTo(strings.TrimSpace(sb.String()), 600), used
}

func trimTo(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}
