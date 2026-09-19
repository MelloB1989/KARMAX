package chatlog

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/MelloB1989/karmax/internal/harness"
)

type Conversation struct {
	ID      string    `json:"id"`
	Title   string    `json:"title"`
	Opening string    `json:"opening"`
	Updated time.Time `json:"updated"`
}

// Message is one turn as a client needs it. ToolCalls are the tool_use blocks
// that were inside that assistant message, so a transcript read back from
// disk shows the same call a live turn showed.
type Message struct {
	Role      string     `json:"role"`
	Text      string     `json:"text"`
	At        time.Time  `json:"at"`
	ToolCalls []ToolCall `json:"toolCalls"`
}

// ToolCall is one tool the assistant invoked, as a transcript remembers it.
//
// The same shape a live turn streams, so a reopened conversation and a running
// one describe the same work in the same words.
type ToolCall struct {
	ID    string `json:"id"`
	Title string `json:"title,omitempty"`
	Kind  string `json:"kind,omitempty"`
	// Status starts "completed" — a transcript has no live calls — and only
	// moves to "failed" once a later record's tool_result names this call's
	// id with is_error true. See applyToolResults.
	Status string          `json:"status"`
	Input  json.RawMessage `json:"input,omitempty"`
	Output string          `json:"output,omitempty"`
}

// record is the subset of the CLI's line format this package reads.
type record struct {
	Type      string `json:"type"`
	AITitle   string `json:"aiTitle"`
	Timestamp string `json:"timestamp"`
	Message   struct {
		Role    string  `json:"role"`
		Content content `json:"content"`
	} `json:"message"`
}

type part struct {
	Type  string          `json:"type"`
	Text  string          `json:"text"`
	ID    string          `json:"id"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`

	// tool_result only — a call's fate, read from a LATER record than the
	// tool_use that started it. See applyToolResults.
	ToolUseID string          `json:"tool_use_id"`
	IsError   bool            `json:"is_error"`
	Content   json.RawMessage `json:"content"`
}

// content is a turn's body, which the CLI writes in two shapes.
//
// A plain string is the common one for something a person typed; a list of
// parts is what an assistant turn and a tool result use. Decoding only the
// list lost every string-shaped turn outright — the message had no text, so it
// was dropped as empty, and a restored transcript was missing the person's own
// words.
type content []part

func (c *content) UnmarshalJSON(b []byte) error {
	var parts []part
	if err := json.Unmarshal(b, &parts); err == nil {
		*c = parts
		return nil
	}
	var text string
	if err := json.Unmarshal(b, &text); err != nil {
		// Neither shape. A turn nobody can read is not a reason to abandon the
		// rest of the transcript.
		*c = nil
		return nil
	}
	*c = content{{Type: "text", Text: text}}
	return nil
}

// scan walks one session file, handing each decoded record to fn.
func scan(path string, fn func(record)) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	// A single line carries a whole message and can be large; the 64KB default
	// would truncate one and take the rest of the transcript with it.
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for sc.Scan() {
		var r record
		if json.Unmarshal(sc.Bytes(), &r) != nil {
			continue
		}
		fn(r)
	}
	return sc.Err()
}

// sessionPath is the file for one conversation id, or "" if the id is not one.
//
// The id reaches this package from an HTTP path and is joined onto a
// directory, so "../../etc/passwd" would otherwise escape the sessions
// directory entirely. A session id is a uuid: anything carrying a separator
// is not one, and is refused rather than cleaned into something plausible.
func sessionPath(dir, id string) string {
	if id == "" || strings.ContainsAny(id, `/\`) || strings.Contains(id, "..") {
		return ""
	}
	return filepath.Join(dir, id+".jsonl")
}

// toolResult is one tool_use call's outcome, read off whichever later record
// carries its tool_result.
type toolResult struct {
	output string
	failed bool
}

func Read(dir, id string) ([]Message, error) {
	path := sessionPath(dir, id)
	if path == "" {
		return nil, fmt.Errorf("chatlog: %q is not a conversation id", id)
	}
	var out []Message
	// Keyed by tool_use id. Filled in as tool_result records are seen, but
	// only APPLIED once the whole file has been read (applyToolResults) —
	// never inline, because the assistant-turn merge below can still append
	// more calls to a Message already in `out`, and a pointer taken into its
	// ToolCalls slice before such an append can be left pointing at a backing
	// array the append has already abandoned.
	results := map[string]toolResult{}
	err := scan(path, func(r record) {
		if r.Type != "user" && r.Type != "assistant" {
			return
		}
		msg := Message{Role: r.Type, At: parseTime(r.Timestamp), ToolCalls: []ToolCall{}}
		for _, c := range r.Message.Content {
			switch c.Type {
			case "text":
				msg.Text = joinProse(msg.Text, c.Text)
			case "tool_use":
				msg.ToolCalls = append(msg.ToolCalls, ToolCall{
					ID:     c.ID,
					Title:  harness.ToolTitle(c.Name, c.Input),
					Kind:   string(harness.ToolKindOf(c.Name)),
					Status: "completed",
					Input:  harness.TruncateToolInput(c.Input),
				})
			case "tool_result":
				// Never itself part of a message — see the empty check below,
				// which is what keeps a user record holding only tool_results
				// from becoming a bubble of its own.
				results[c.ToolUseID] = toolResult{
					output: harness.TruncateOutput(harness.ToolResultText(c.Content)),
					failed: c.IsError,
				}
			}
		}
		if msg.Text == "" && len(msg.ToolCalls) == 0 {
			return
		}
		// The CLI splits one assistant turn across several records — tools in
		// one, prose in the next. Merging them keeps a turn a turn. User
		// records never split this way, so merging those would instead fuse
		// two separate things the person typed into one bubble.
		if n := len(out); n > 0 && msg.Role == "assistant" && out[n-1].Role == msg.Role {
			out[n-1].Text = joinProse(out[n-1].Text, msg.Text)
			out[n-1].ToolCalls = append(out[n-1].ToolCalls, msg.ToolCalls...)
			return
		}
		out = append(out, msg)
	})
	if err != nil {
		return nil, err
	}
	applyToolResults(out, results)
	return out, nil
}

// applyToolResults folds each tool_result's fate into the call it answers, by
// id, now that no ToolCalls slice in out will grow again — see Read's own
// comment on why this cannot happen inline as each record is scanned.
func applyToolResults(out []Message, results map[string]toolResult) {
	for i := range out {
		calls := out[i].ToolCalls
		for j := range calls {
			res, ok := results[calls[j].ID]
			if !ok {
				continue
			}
			calls[j].Output = res.output
			if res.failed {
				calls[j].Status = "failed"
			}
		}
	}
}

// joinProse puts a paragraph break between two blocks of a turn's prose.
//
// The CLI writes the prose either side of each tool call as a block of its
// own, and a block carries no separator: joined bare, "Let me check." and
// "## Root cause" become "Let me check.## Root cause" — a sentence nobody
// wrote, and a heading no renderer can find.
func joinProse(a, b string) string {
	if a == "" || b == "" {
		return a + b
	}
	return strings.TrimRight(a, " \t\n") + "\n\n" + strings.TrimLeft(b, "\n")
}

func List(dir string) ([]Conversation, error) {
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return []Conversation{}, nil // nobody has chatted here yet
	}
	if err != nil {
		return nil, err
	}
	out := []Conversation{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		id := strings.TrimSuffix(e.Name(), ".jsonl")
		c := Conversation{ID: id}
		if info, err := e.Info(); err == nil {
			c.Updated = info.ModTime()
		}
		_ = scan(filepath.Join(dir, e.Name()), func(r record) {
			if r.Type == "ai-title" && c.Title == "" {
				c.Title = r.AITitle
			}
			if r.Type == "user" && c.Opening == "" {
				for _, part := range r.Message.Content {
					if part.Type == "text" {
						c.Opening = trim(part.Text, 140)
						break
					}
				}
			}
		})
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Updated.After(out[j].Updated) })
	return out, nil
}

func Delete(dir, id string) error {
	path := sessionPath(dir, id)
	if path == "" {
		return fmt.Errorf("chatlog: %q is not a conversation id", id)
	}
	err := os.Remove(path)
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

func parseTime(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}
	}
	return t
}

func trim(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
