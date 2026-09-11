package chatlog

import (
	"path/filepath"
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
	if len(msgs[1].Steps) != 1 || msgs[1].Steps[0].Tool != "Bash" {
		t.Errorf("steps = %+v, want one Bash step", msgs[1].Steps)
	}
}

// A record type this build has never seen is skipped, not an error. The format
// belongs to another program and will grow types without asking.
func TestUnknownRecordTypesAreSkipped(t *testing.T) {
	if _, err := Read("testdata", "session-basic"); err != nil {
		t.Fatalf("an unknown record type broke the read: %v", err)
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
