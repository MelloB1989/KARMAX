package harness

import (
	"encoding/json"
	"net/url"
	"path"
	"strings"
)

// ToolKind is what sort of thing a tool does, in ACP's vocabulary.
//
// The point of a kind is that a transcript can pick an icon and group
// consecutive calls without parsing tool names it has never heard of.
type ToolKind string

const (
	ToolRead       ToolKind = "read"
	ToolEdit       ToolKind = "edit"
	ToolDelete     ToolKind = "delete"
	ToolMove       ToolKind = "move"
	ToolSearch     ToolKind = "search"
	ToolExecute    ToolKind = "execute"
	ToolThink      ToolKind = "think"
	ToolFetch      ToolKind = "fetch"
	ToolSwitchMode ToolKind = "switch_mode"
	ToolOther      ToolKind = "other"
)

// Status is where a tool call has got to, in ACP's vocabulary.
type Status string

const (
	StatusPending    Status = "pending"
	StatusInProgress Status = "in_progress"
	StatusCompleted  Status = "completed"
	StatusFailed     Status = "failed"
)

// Location is a file a tool call touched, and where in it.
type Location struct {
	Path string `json:"path"`
	Line int    `json:"line,omitempty"`
}

// titleMax is where a title stops being a label and starts being a paragraph.
const titleMax = 60

func toolKind(name string) ToolKind {
	switch name {
	case "Read", "NotebookRead":
		return ToolRead
	case "Edit", "Write", "MultiEdit", "NotebookEdit":
		return ToolEdit
	case "Bash", "BashOutput", "KillShell", "KillBash":
		return ToolExecute
	case "Glob", "Grep", "LS":
		return ToolSearch
	case "WebFetch", "WebSearch":
		return ToolFetch
	case "Task":
		return ToolThink
	}
	// MCP tools arrive as mcp__<server>__<tool>. A browser driven by one is
	// reaching outside this machine, which is what "fetch" means to a reader.
	if strings.HasPrefix(name, "mcp__") && strings.Contains(name, "browser") {
		return ToolFetch
	}
	return ToolOther
}

// toolInput is every field any titled tool reads. One struct rather than one
// per tool: the CLI ignores unknown keys and so can we.
type toolInput struct {
	Description  string `json:"description"`
	Command      string `json:"command"`
	FilePath     string `json:"file_path"`
	NotebookPath string `json:"notebook_path"`
	Path         string `json:"path"`
	Pattern      string `json:"pattern"`
	URL          string `json:"url"`
	Query        string `json:"query"`
	Prompt       string `json:"prompt"`
	Offset       int    `json:"offset"`
}

func parseInput(input json.RawMessage) toolInput {
	var in toolInput
	// A tool with no input, or one whose input is not an object, is not an
	// error worth losing the call over — it just has no title of its own.
	_ = json.Unmarshal(input, &in)
	return in
}

// toolTitle is what the transcript writes on the line, in the agent's own
// words wherever it gave any.
//
// Never empty: the desktop draws an icon and this string, and a blank line
// reads as a bug rather than as a tool nobody has taught us about.
func toolTitle(name string, input json.RawMessage) string {
	in := parseInput(input)
	if s := clip(in.Description); s != "" {
		return s
	}
	switch toolKind(name) {
	case ToolRead, ToolEdit:
		if p := firstPath(in.FilePath, in.NotebookPath, in.Path); p != "" {
			return path.Base(p)
		}
	case ToolExecute:
		if s := clip(in.Command); s != "" {
			return s
		}
	case ToolSearch:
		if s := clip(in.Pattern); s != "" {
			return s
		}
		if p := firstPath(in.Path, in.FilePath); p != "" {
			return path.Base(p)
		}
	case ToolFetch:
		if in.URL != "" {
			if u, err := url.Parse(in.URL); err == nil && u.Host != "" {
				return u.Host
			}
			return clip(in.URL)
		}
		if s := clip(in.Query); s != "" {
			return s
		}
	case ToolThink:
		if s := clip(in.Prompt); s != "" {
			return s
		}
	}
	if name == "" {
		return "tool"
	}
	return name
}

// toolLocations is the files a call touched, so a turn can say where it went.
func toolLocations(name string, input json.RawMessage) []Location {
	in := parseInput(input)
	p := firstPath(in.FilePath, in.NotebookPath)
	if p == "" && toolKind(name) == ToolSearch {
		p = in.Path
	}
	if p == "" {
		return nil
	}
	return []Location{{Path: p, Line: in.Offset}}
}

// clip reduces a value to one line of at most titleMax runes.
//
// Rune-wise, not byte-wise: cutting a multi-byte character in half puts U+FFFD
// on screen.
func clip(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = strings.TrimSpace(s[:i])
	}
	r := []rune(s)
	if len(r) > titleMax {
		return string(r[:titleMax])
	}
	return s
}

func firstPath(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
