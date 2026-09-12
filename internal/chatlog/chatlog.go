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
)

type Conversation struct {
	ID      string    `json:"id"`
	Title   string    `json:"title"`
	Opening string    `json:"opening"`
	Updated time.Time `json:"updated"`
}

// Message is one turn as a client needs it. Steps are the tool_use blocks that
// were inside that assistant message, so a transcript read back from disk
// shows the same step line a live turn showed.
type Message struct {
	Role  string    `json:"role"`
	Text  string    `json:"text"`
	At    time.Time `json:"at"`
	Steps []Step    `json:"steps"`
}

type Step struct {
	Tool   string `json:"tool"`
	Phase  string `json:"phase"` // always "done": a transcript has no live steps
	Detail string `json:"detail"`
}

// record is the subset of the CLI's line format this package reads.
type record struct {
	Type      string `json:"type"`
	AITitle   string `json:"aiTitle"`
	Timestamp string `json:"timestamp"`
	Message   struct {
		Role    string `json:"role"`
		Content []struct {
			Type  string          `json:"type"`
			Text  string          `json:"text"`
			Name  string          `json:"name"`
			Input json.RawMessage `json:"input"`
		} `json:"content"`
	} `json:"message"`
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

func Read(dir, id string) ([]Message, error) {
	path := sessionPath(dir, id)
	if path == "" {
		return nil, fmt.Errorf("chatlog: %q is not a conversation id", id)
	}
	var out []Message
	err := scan(path, func(r record) {
		if r.Type != "user" && r.Type != "assistant" {
			return
		}
		msg := Message{Role: r.Type, At: parseTime(r.Timestamp), Steps: []Step{}}
		for _, c := range r.Message.Content {
			switch c.Type {
			case "text":
				msg.Text += c.Text
			case "tool_use":
				msg.Steps = append(msg.Steps, Step{Tool: c.Name, Phase: "done"})
			}
		}
		if msg.Text == "" && len(msg.Steps) == 0 {
			return
		}
		// The CLI splits one assistant turn across several records — tools in
		// one, prose in the next. Merging them keeps a turn a turn. User
		// records never split this way, so merging those would instead fuse
		// two separate things the person typed into one bubble.
		if n := len(out); n > 0 && msg.Role == "assistant" && out[n-1].Role == msg.Role {
			out[n-1].Text += msg.Text
			out[n-1].Steps = append(out[n-1].Steps, msg.Steps...)
			return
		}
		out = append(out, msg)
	})
	return out, err
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
