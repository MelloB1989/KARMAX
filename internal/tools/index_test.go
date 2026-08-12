package tools

import (
	"strings"
	"testing"
)

func TestSummarizeTakesTheFirstSentence(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"Send a message. Then do other things.", "Send a message."},
		{"Add a reminder to the operator's phone (iOS Reminders). Optional due date.", "Add a reminder to the operator's phone (iOS Reminders)."},
		{"", ""},
		{"No terminator here", "No terminator here"},
		// A dot inside a tool name is not a sentence end.
		{"Use comms.send to deliver it. And more.", "Use comms.send to deliver it."},
		// Abbreviations are not sentence ends either.
		{"Manage files, e.g. read and write. Second sentence.", "Manage files, e.g. read and write."},
		// Multi-line descriptions collapse to one line.
		{"First line\n\nSecond line.", "First line Second line."},
	} {
		if got := Summarize(tc.in); got != tc.want {
			t.Errorf("Summarize(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestSummarizeBoundsLongText(t *testing.T) {
	long := strings.Repeat("word ", 100)
	got := Summarize(long)
	if len(got) > 145 {
		t.Errorf("summary is %d chars, want bounded", len(got))
	}
	if !strings.HasSuffix(got, "…") {
		t.Errorf("a truncated summary should say so: %q", got)
	}
}

// The index must be byte-identical between turns or it can never be cached.
func TestIndexIsStableAndOnePerLine(t *testing.T) {
	in := []ToolManifest{
		{Name: "zeta.tool", Description: "Last alphabetically. Extra detail."},
		{Name: "alpha.tool", Description: "First alphabetically."},
	}
	got := Index(in)

	shuffled := []ToolManifest{in[1], in[0]}
	if Index(shuffled) != got {
		t.Error("index is not order-stable; a reordering prefix defeats caching")
	}

	lines := strings.Split(strings.TrimSpace(got), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected one line per tool, got %d: %q", len(lines), got)
	}
	if !strings.HasPrefix(lines[0], "- alpha.tool: First alphabetically.") {
		t.Errorf("first line = %q", lines[0])
	}
	if strings.Contains(got, "Extra detail") {
		t.Error("the index should carry only the first sentence")
	}
}

func TestEmptyIndex(t *testing.T) {
	if Index(nil) != "" {
		t.Error("an empty tool set should produce no index")
	}
}
