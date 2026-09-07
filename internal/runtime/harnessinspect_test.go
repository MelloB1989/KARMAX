package runtime

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The transcript is the only record of what a session with a real shell
// actually did, so finding it has to be right. The CLI flattens every
// separator in the working directory to a dash.
func TestTheTranscriptIsFoundWhereTheCLIPutsIt(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	workdir := "/home/n/.karmax/sessions/agent_nexus"
	id := "7d038337-e2a5-4008-a8f5-1eca783d6d5d"
	dir := filepath.Join(home, ".claude", "projects", "-home-n--karmax-sessions-agent-nexus")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(dir, id+".jsonl")
	if err := os.WriteFile(want, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if got := transcriptPath(workdir, id); got != want {
		t.Errorf("transcriptPath = %q, want %q", got, want)
	}
}

// A missing transcript is an empty answer, not a path that does not exist:
// handing back a bogus path makes the caller open it and fail further away.
func TestAMissingTranscriptReportsNothingRatherThanAGuess(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if got := transcriptPath("/nowhere/at/all", "no-such-id"); got != "" {
		t.Errorf("got %q for a transcript that does not exist", got)
	}
	if got := transcriptPath("", ""); got != "" {
		t.Errorf("got %q for an empty session", got)
	}
}

// Only the last N entries, whatever the size of the file. A busy session's
// transcript reaches megabytes, and reading all of it to show twenty lines is
// how a status command becomes one you avoid running.
func TestOnlyTheTailIsKept(t *testing.T) {
	path := filepath.Join(t.TempDir(), "t.jsonl")
	var b strings.Builder
	for i := 0; i < 500; i++ {
		b.WriteString(`{"type":"user","message":{"role":"user","content":"hello"}}` + "\n")
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := readTranscript(path, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 5 {
		t.Errorf("kept %d entries, want 5", len(got))
	}
}

// Tool calls are the interesting half of what a session did. A turn that ran
// commands and said nothing must not vanish from the record.
func TestToolCallsSurviveIntoTheRecord(t *testing.T) {
	path := filepath.Join(t.TempDir(), "t.jsonl")
	lines := `{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"running it"},{"type":"tool_use","name":"Bash"}]}}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","name":"Read"}]}}
{"type":"summary","summary":"ignored"}
`
	if err := os.WriteFile(path, []byte(lines), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := readTranscript(path, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d entries, want 2 (the summary line is not an exchange)", len(got))
	}
	if got[0].Text != "running it" || len(got[0].Tools) != 1 || got[0].Tools[0] != "Bash" {
		t.Errorf("first entry lost its text or its tool: %+v", got[0])
	}
	// A turn that only ran a tool still happened.
	if len(got[1].Tools) != 1 || got[1].Tools[0] != "Read" {
		t.Errorf("a silent tool-only turn was dropped: %+v", got[1])
	}
}

// An unparseable line must not end the read: one malformed entry would
// otherwise hide every exchange after it.
func TestOneBadLineDoesNotHideTheRest(t *testing.T) {
	path := filepath.Join(t.TempDir(), "t.jsonl")
	lines := `{"type":"user","message":{"role":"user","content":"first"}}
not json at all
{"type":"user","message":{"role":"user","content":"second"}}
`
	if err := os.WriteFile(path, []byte(lines), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := readTranscript(path, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Errorf("got %d entries; a malformed line hid the ones after it", len(got))
	}
}
