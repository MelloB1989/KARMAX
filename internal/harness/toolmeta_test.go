package harness

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestToolKindClassifiesTheToolsWeActuallySee(t *testing.T) {
	cases := map[string]ToolKind{
		"Read":                              ToolRead,
		"NotebookRead":                      ToolRead,
		"Edit":                              ToolEdit,
		"Write":                             ToolEdit,
		"NotebookEdit":                      ToolEdit,
		"Bash":                              ToolExecute,
		"BashOutput":                        ToolExecute,
		"Glob":                              ToolSearch,
		"Grep":                              ToolSearch,
		"WebFetch":                          ToolFetch,
		"WebSearch":                         ToolFetch,
		"Task":                              ToolThink,
		"mcp__playwright__browser_navigate": ToolFetch,
		"mcp__whatever__something":          ToolOther,
		"SomethingNobodyHasWrittenYet":      ToolOther,
	}
	for name, want := range cases {
		if got := toolKind(name); got != want {
			t.Errorf("toolKind(%q) = %q, want %q", name, got, want)
		}
	}
}

// The agent's own description beats anything we could invent for it.
func TestToolTitlePrefersTheAgentsDescription(t *testing.T) {
	in := json.RawMessage(`{"command":"go test ./...","description":"Check the tests pass"}`)
	if got := toolTitle("Bash", in); got != "Check the tests pass" {
		t.Errorf("got %q, want the description", got)
	}
}

// Without one, the command itself says more than the word "Bash" does.
func TestToolTitleFallsBackToTheCommand(t *testing.T) {
	in := json.RawMessage(`{"command":"go build ./..."}`)
	if got := toolTitle("Bash", in); got != "go build ./..." {
		t.Errorf("got %q, want the command", got)
	}
}

// A long command must not push the reply off the screen.
func TestToolTitleTrimsALongCommandToOneLine(t *testing.T) {
	in := json.RawMessage(`{"command":"one\ntwo\nthree"}`)
	if got := toolTitle("Bash", in); got != "one" {
		t.Errorf("got %q, want only the first line", got)
	}
	long, _ := json.Marshal(map[string]string{"command": strings.Repeat("x", 200)})
	got := toolTitle("Bash", long)
	if len([]rune(got)) != 60 {
		t.Errorf("got %d runes, want 60", len([]rune(got)))
	}
}

// A file's base name is what a person recognises; the full path is noise.
func TestToolTitleNamesTheFile(t *testing.T) {
	in := json.RawMessage(`{"file_path":"/Users/x/code/app/src/main.ts"}`)
	if got := toolTitle("Read", in); got != "main.ts" {
		t.Errorf("got %q, want the base name", got)
	}
}

func TestToolTitleUsesPatternHostAndQuery(t *testing.T) {
	if got := toolTitle("Grep", json.RawMessage(`{"pattern":"func main"}`)); got != "func main" {
		t.Errorf("Grep: got %q", got)
	}
	if got := toolTitle("WebFetch", json.RawMessage(`{"url":"https://example.com/a/b?c=d"}`)); got != "example.com" {
		t.Errorf("WebFetch: got %q", got)
	}
	if got := toolTitle("WebSearch", json.RawMessage(`{"query":"go generics"}`)); got != "go generics" {
		t.Errorf("WebSearch: got %q", got)
	}
}

// Never empty: a blank line in the transcript is worse than honest jargon.
func TestToolTitleFallsBackToTheToolName(t *testing.T) {
	if got := toolTitle("Mystery", json.RawMessage(`{}`)); got != "Mystery" {
		t.Errorf("got %q, want the tool name", got)
	}
	if got := toolTitle("Mystery", nil); got != "Mystery" {
		t.Errorf("nil input: got %q, want the tool name", got)
	}
	if got := toolTitle("Mystery", json.RawMessage(`not json at all`)); got != "Mystery" {
		t.Errorf("bad json: got %q, want the tool name", got)
	}
}

func TestToolLocationsCarryTheFileAndLine(t *testing.T) {
	got := toolLocations("Read", json.RawMessage(`{"file_path":"/a/b.go","offset":42}`))
	want := []Location{{Path: "/a/b.go", Line: 42}}
	if len(got) != 1 || got[0] != want[0] {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

// A tool that names no file has no locations — not one empty location.
func TestToolLocationsAreEmptyWhenNoFileIsNamed(t *testing.T) {
	if got := toolLocations("Bash", json.RawMessage(`{"command":"ls"}`)); len(got) != 0 {
		t.Errorf("got %+v, want none", got)
	}
}
