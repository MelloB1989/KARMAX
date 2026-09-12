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
	if len(msgs[2].Steps) != 1 || msgs[2].Steps[0].Tool != "Bash" {
		t.Errorf("steps = %+v, want the assistant's split records still merged", msgs[2].Steps)
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
