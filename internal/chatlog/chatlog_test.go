package chatlog

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A session file is a conversation: its title, its turns, and the tools that
// ran inside them.
func TestReadsATranscript(t *testing.T) {
	msgs, err := Read("testdata", "session-basic")
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 2 {
		t.Fatalf("got %d messages, want 2 (user, assistant)", len(msgs))
	}
	if msgs[0].Role != "user" || msgs[0].Text != "Archive the old screenshots" {
		t.Errorf("first message = %+v", msgs[0])
	}
	if msgs[1].Role != "assistant" || msgs[1].Text != "Found 14, all from July." {
		t.Errorf("second message = %+v", msgs[1])
	}
	if len(msgs[1].ToolCalls) != 1 || msgs[1].ToolCalls[0].Kind != "execute" || msgs[1].ToolCalls[0].Title != "ls ~/Downloads" {
		t.Errorf("tool calls = %+v, want one execute call titled the command", msgs[1].ToolCalls)
	}
}

// A record type this build has never seen is skipped, not an error. The format
// belongs to another program and will grow types without asking.
func TestUnknownRecordTypesAreSkipped(t *testing.T) {
	if _, err := Read("testdata", "session-basic"); err != nil {
		t.Fatalf("an unknown record type broke the read: %v", err)
	}
}

// Merging is for a single assistant turn the CLI split across records — not
// for two separate things the person typed. Two consecutive user records are
// two messages; a tool_use-then-text assistant pair is still one.
func TestUserTurnsDoNotMergeAcrossRecords(t *testing.T) {
	msgs, err := Read("testdata/consecutive-user", "session-consecutive-user")
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 3 {
		t.Fatalf("got %d messages, want 3 (user, user, assistant): %+v", len(msgs), msgs)
	}
	if msgs[0].Role != "user" || msgs[0].Text != "First question" {
		t.Errorf("first message = %+v", msgs[0])
	}
	if msgs[1].Role != "user" || msgs[1].Text != "Second question" {
		t.Errorf("second message = %+v, want a separate user turn", msgs[1])
	}
	if msgs[2].Role != "assistant" || msgs[2].Text != "Done." {
		t.Errorf("third message = %+v", msgs[2])
	}
	if len(msgs[2].ToolCalls) != 1 || msgs[2].ToolCalls[0].Kind != "execute" || msgs[2].ToolCalls[0].Title != "ls" {
		t.Errorf("tool calls = %+v, want the assistant's split records still merged", msgs[2].ToolCalls)
	}
}

// The list is what the conversation column renders.
func TestListsWithTitleAndOpening(t *testing.T) {
	convs, err := List("testdata")
	if err != nil {
		t.Fatal(err)
	}
	if len(convs) != 1 {
		t.Fatalf("got %d conversations, want 1", len(convs))
	}
	c := convs[0]
	if c.ID != "session-basic" {
		t.Errorf("id = %q", c.ID)
	}
	if c.Title != "Archiving old screenshots" {
		t.Errorf("title = %q, want the ai-title record", c.Title)
	}
	if c.Opening != "Archive the old screenshots" {
		t.Errorf("opening = %q, want the first user turn", c.Opening)
	}
	if c.Updated.IsZero() {
		t.Error("updated is zero; the list sorts on it")
	}
}

// The slug is how a working directory becomes a project directory name, and it
// has to match what the CLI itself does or every path is wrong.
func TestSlug(t *testing.T) {
	got := Slug("/Users/x/Developer/code/my_app.v2")
	want := "-Users-x-Developer-code-my-app-v2"
	if got != want {
		t.Fatalf("Slug() = %q, want %q", got, want)
	}
	if Dir("/Users/x") != filepath.Join(homeDir(), ".claude", "projects", "-Users-x") {
		t.Errorf("Dir() = %q", Dir("/Users/x"))
	}
}

// A conversation id reaches this package from an HTTP path, so it must not be
// able to name a file outside the sessions directory.
func TestIdsThatEscapeTheDirectoryAreRefused(t *testing.T) {
	for _, id := range []string{
		"../../etc/passwd",
		"..",
		"sub/dir",
		`back\slash`,
		"",
	} {
		if _, err := Read("testdata", id); err == nil {
			t.Errorf("Read accepted %q", id)
		}
		if err := Delete("testdata", id); err == nil {
			t.Errorf("Delete accepted %q", id)
		}
	}
	// The real thing still works.
	if _, err := Read("testdata", "session-basic"); err != nil {
		t.Fatalf("the guard refused a real id: %v", err)
	}
}

// The CLI writes a turn's body as a plain string as often as a list of parts.
//
// Reading only the list shape dropped every string-shaped turn as empty, so a
// restored transcript was silently missing the person's own words — and an
// untitled conversation had no opening line to show in a list either.
func TestStringShapedContentIsRead(t *testing.T) {
	msgs, err := Read("testdata/string-content", "plain")
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 2 {
		t.Fatalf("got %d messages, want 2 — a string-shaped turn was dropped", len(msgs))
	}
	if msgs[0].Role != "user" || msgs[0].Text != "Plain string, the way a person's own turn is usually written" {
		t.Errorf("string-shaped turn came back as %+v", msgs[0])
	}
	if msgs[1].Text != "A list of parts, the way a reply is written." {
		t.Errorf("list-shaped turn regressed: %+v", msgs[1])
	}

	// An untitled conversation leans on the opening line to be tellable apart
	// from every other untitled one in the list.
	convs, err := List("testdata/string-content")
	if err != nil {
		t.Fatal(err)
	}
	if len(convs) != 1 || convs[0].Opening == "" {
		t.Fatalf("no opening line for an untitled conversation: %+v", convs)
	}
}

// A reopened conversation must still show what the assistant did, with the
// same vocabulary a live turn uses. Emitting the old {tool, phase} shape here
// while the wire emits tool calls is how history silently loses its work.
func TestHistoryCarriesWholeToolCalls(t *testing.T) {
	dir := t.TempDir()
	line := `{"type":"assistant","timestamp":"2026-09-13T10:00:00Z","message":{"content":[` +
		`{"type":"tool_use","id":"toolu_9","name":"Read","input":{"file_path":"/a/main.go"}}]}}`
	if err := os.WriteFile(filepath.Join(dir, "c1.jsonl"), []byte(line+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	msgs, err := Read(dir, "c1")
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 {
		t.Fatalf("got %d messages, want 1", len(msgs))
	}
	calls := msgs[0].ToolCalls
	if len(calls) != 1 {
		t.Fatalf("got %d tool calls, want 1", len(calls))
	}
	if calls[0].ID != "toolu_9" {
		t.Errorf("id = %q, want toolu_9", calls[0].ID)
	}
	if calls[0].Title != "main.go" {
		t.Errorf("title = %q, want main.go", calls[0].Title)
	}
	if calls[0].Kind != "read" {
		t.Errorf("kind = %q, want read", calls[0].Kind)
	}
	// A transcript has no live calls: everything in it already finished.
	if calls[0].Status != "completed" {
		t.Errorf("status = %q, want completed", calls[0].Status)
	}
}

// Two tool calls interleaved with their own results — call, result, call,
// result — must each resolve to the right one, and the assistant records
// either side of the (skipped) user records must still merge into one turn.
func TestHistoryAttachesOutputAndStatusToTheRightCall(t *testing.T) {
	dir := t.TempDir()
	lines := `{"type":"assistant","timestamp":"2026-09-14T10:00:00Z","message":{"content":[` +
		`{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"ls"}}]}}` + "\n" +
		`{"type":"user","timestamp":"2026-09-14T10:00:01Z","message":{"content":[` +
		`{"type":"tool_result","tool_use_id":"t1","is_error":false,"content":"file1\nfile2"}]}}` + "\n" +
		`{"type":"assistant","timestamp":"2026-09-14T10:00:02Z","message":{"content":[` +
		`{"type":"tool_use","id":"t2","name":"Bash","input":{"command":"false"}}]}}` + "\n" +
		`{"type":"user","timestamp":"2026-09-14T10:00:03Z","message":{"content":[` +
		`{"type":"tool_result","tool_use_id":"t2","is_error":true,"content":"no such command"}]}}` + "\n" +
		`{"type":"assistant","timestamp":"2026-09-14T10:00:04Z","message":{"content":[` +
		`{"type":"text","text":"Done."}]}}` + "\n"
	if err := os.WriteFile(filepath.Join(dir, "c1.jsonl"), []byte(lines), 0o644); err != nil {
		t.Fatal(err)
	}

	msgs, err := Read(dir, "c1")
	if err != nil {
		t.Fatal(err)
	}
	// The user records carried nothing but tool_results, so they must not have
	// become messages of their own — every assistant record merges into one.
	if len(msgs) != 1 || msgs[0].Role != "assistant" || msgs[0].Text != "Done." {
		t.Fatalf("got %+v, want one assistant turn merged across the tool_result records", msgs)
	}
	calls := msgs[0].ToolCalls
	if len(calls) != 2 {
		t.Fatalf("got %d tool calls, want 2", len(calls))
	}
	if calls[0].ID != "t1" || calls[0].Status != "completed" || calls[0].Output != "file1\nfile2" {
		t.Errorf("first call = %+v, want t1 completed with its own output", calls[0])
	}
	if calls[1].ID != "t2" || calls[1].Status != "failed" || calls[1].Output != "no such command" {
		t.Errorf("second call = %+v, want t2 failed with its own output", calls[1])
	}
}

// A tool_use's input reaches the client the same way its live counterpart
// does: as JSON, with any long string field cut at 4000 runes.
func TestToolCallInputIsTruncated(t *testing.T) {
	dir := t.TempDir()
	long := strings.Repeat("a", 4500)
	line := `{"type":"assistant","timestamp":"2026-09-14T10:00:00Z","message":{"content":[` +
		`{"type":"tool_use","id":"t1","name":"Write","input":{"file_path":"/a.txt","content":"` + long + `"}}]}}` + "\n"
	if err := os.WriteFile(filepath.Join(dir, "c1.jsonl"), []byte(line), 0o644); err != nil {
		t.Fatal(err)
	}

	msgs, err := Read(dir, "c1")
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 || len(msgs[0].ToolCalls) != 1 {
		t.Fatalf("got %+v", msgs)
	}
	var in map[string]any
	if err := json.Unmarshal(msgs[0].ToolCalls[0].Input, &in); err != nil {
		t.Fatalf("input is not json: %v", err)
	}
	r := []rune(in["content"].(string))
	if len(r) != 4001 || r[len(r)-1] != '…' {
		t.Errorf("content not truncated: got %d runes", len(r))
	}
	if in["file_path"] != "/a.txt" {
		t.Errorf("an untouched field changed: %+v", in["file_path"])
	}
}

// A tool result can be a whole file. history caps it exactly as a live turn
// does, so a giant result never bloats a conversation's history payload.
func TestToolCallOutputIsCapped(t *testing.T) {
	dir := t.TempDir()
	long := strings.Repeat("x", 20000)
	lines := `{"type":"assistant","timestamp":"2026-09-14T10:00:00Z","message":{"content":[` +
		`{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"cat big.txt"}}]}}` + "\n" +
		`{"type":"user","timestamp":"2026-09-14T10:00:01Z","message":{"content":[` +
		`{"type":"tool_result","tool_use_id":"t1","content":"` + long + `"}]}}` + "\n"
	if err := os.WriteFile(filepath.Join(dir, "c1.jsonl"), []byte(lines), 0o644); err != nil {
		t.Fatal(err)
	}

	msgs, err := Read(dir, "c1")
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 || len(msgs[0].ToolCalls) != 1 {
		t.Fatalf("got %+v", msgs)
	}
	out := msgs[0].ToolCalls[0].Output
	if len(out) > 16000+len("\n…") {
		t.Errorf("output not capped: got %d bytes", len(out))
	}
	if !strings.HasSuffix(out, "\n…") {
		t.Errorf("no truncation marker on a capped output: %q", out[max(0, len(out)-10):])
	}
}

// Prose either side of a tool call is two paragraphs, whether the CLI put the
// blocks in one record or split the turn across several.
func TestProseAcrossToolCallsKeepsItsParagraphs(t *testing.T) {
	dir := t.TempDir()
	lines := `{"type":"assistant","timestamp":"2026-09-14T10:00:00Z","message":{"content":[` +
		`{"type":"text","text":"Let me check."},` +
		`{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"ls"}},` +
		`{"type":"text","text":"Found the logs."}]}}` + "\n" +
		`{"type":"assistant","timestamp":"2026-09-14T10:00:02Z","message":{"content":[` +
		`{"type":"text","text":"## Root cause"}]}}` + "\n"
	if err := os.WriteFile(filepath.Join(dir, "c1.jsonl"), []byte(lines), 0o644); err != nil {
		t.Fatal(err)
	}
	msgs, err := Read(dir, "c1")
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 {
		t.Fatalf("got %d messages, want 1", len(msgs))
	}
	want := "Let me check.\n\nFound the logs.\n\n## Root cause"
	if msgs[0].Text != want {
		t.Errorf("text = %q, want %q", msgs[0].Text, want)
	}
}
