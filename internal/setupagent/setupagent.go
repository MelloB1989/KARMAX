// Package setupagent connects a service by doing the clicking.
//
// Some services are one command — WhatsApp is a QR code, and that is the whole
// of it. Others are twenty minutes in a console: create a project, enable six
// APIs, fill in a consent screen, make a client, download a JSON file, hand it
// to a CLI, then authorize an account against it. Every one of those steps is a
// page in a browser, and every one of them is why somebody gives up before
// their calendar is connected.
//
// So an agent does it, in the browser the operator is already signed into (see
// internal/browser), narrating as it goes. It stops at the steps only a person
// can take — the Allow button on a consent screen is one — and says so, rather
// than stalling silently at a page nobody is looking at.
//
// The recipes live here rather than in a connector because they are not part of
// what a connector IS. A connector knows how to hold a credential and check it
// still works; this knows how to get one in the first place, which is a
// different question with a different answer per service and a shorter shelf
// life. Nothing in the daemon depends on a recipe existing.
package setupagent

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/MelloB1989/karmax/internal/browser"
	"github.com/MelloB1989/karmax/internal/fsscope"
)

// Recipe is how one service gets connected.
type Recipe struct {
	// ID matches the integration it connects, so a caller that has a tile on a
	// screen knows which button this is behind.
	ID   string
	Name string

	// Needs are binaries that must be on the machine first. A recipe that
	// would spend five minutes in a console and then fail because gog is not
	// installed has wasted somebody's afternoon.
	Needs []string

	// Lede is what to say while it starts, before the agent has said anything.
	Lede string

	// Prompt is the instruction set, given the resolved paths.
	Prompt func(Env) string
}

// Env is what a prompt is allowed to know about this machine.
type Env struct {
	// Bin holds the resolved paths of everything in Needs, keyed by name.
	Bin map[string]string
	// Account is the address the operator wants connected, when they gave one.
	Account string
}

// Progress is one thing worth telling somebody while this runs.
type Progress struct {
	// Kind is "starting", "step", "doing", "says", "needs-you", "done" or
	// "failed". A caller that does not recognise one should show it as a step.
	Kind string `json:"kind"`
	Text string `json:"text"`
}

// Options are the knobs a caller sets.
type Options struct {
	// DataDir is KARMAX's own, for the access policy.
	DataDir string
	// Browser is the window this runs in. Required: a recipe with no browser is
	// an agent reading instructions it cannot follow.
	Browser *browser.Session
	// Account is passed through to the prompt.
	Account string
	// Timeout bounds the whole run. Zero means twenty minutes, which is longer
	// than any of these should take and shorter than somebody's patience.
	Timeout time.Duration
}

// Run works through a recipe, reporting as it goes.
//
// onProgress is called from this goroutine, in order, and must not block for
// long — it is on the path between the agent doing something and the person
// being told about it.
func Run(ctx context.Context, r Recipe, opts Options, onProgress func(Progress)) error {
	say := reporter(onProgress)

	if opts.Browser == nil {
		return errors.New("this needs the browser, and none was given")
	}

	env := Env{Bin: map[string]string{}, Account: strings.TrimSpace(opts.Account)}
	for _, need := range r.Needs {
		path, err := exec.LookPath(need)
		if err != nil {
			return fmt.Errorf("%s needs %s on this machine first", r.Name, need)
		}
		env.Bin[need] = path
	}

	say("starting", r.Lede)
	if err := opts.Browser.Start(ctx); err != nil {
		return fmt.Errorf("opening the browser: %w", err)
	}
	mcp, err := opts.Browser.MCPConfigJSON(ctx)
	if err != nil {
		return fmt.Errorf("attaching the browser: %w", err)
	}

	timeout := opts.Timeout
	if timeout == 0 {
		timeout = 20 * time.Minute
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// stream-json rather than text, because the whole point is that somebody
	// can watch. --verbose is what the CLI requires to stream in print mode.
	args := []string{"--print", "--output-format", "stream-json", "--verbose"}
	policy := fsscope.Load(opts.DataDir)
	if settings := policy.SettingsJSON(opts.DataDir); settings != "" {
		args = append(args, "--permission-mode", "dontAsk", "--settings", settings)
	} else {
		args = append(args, "--dangerously-skip-permissions")
	}
	args = append(args, r.Prompt(env))
	// After the prompt: --mcp-config is variadic and eats whatever follows it.
	args = append(args, "--mcp-config", mcp)

	cmd := exec.CommandContext(runCtx, "claude", args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	cmd.Stderr = nil
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("starting the assistant: %w", err)
	}

	failure := ""
	scanner := bufio.NewScanner(stdout)
	// A snapshot of a page is a large thing to arrive on one line.
	scanner.Buffer(make([]byte, 0, 64<<10), 8<<20)
	for scanner.Scan() {
		if p, ok := readEvent(scanner.Bytes()); ok {
			if p.Kind == "failed" {
				failure = p.Text
			}
			say(p.Kind, p.Text)
		}
	}
	if err := cmd.Wait(); err != nil {
		if failure == "" {
			failure = err.Error()
		}
		return errors.New(failure)
	}
	if failure != "" {
		return errors.New(failure)
	}
	return nil
}

// reporter is how everything in a run reaches the person watching.
//
// It drops empties, and it swallows a closing summary that repeats what was
// just said: the last thing an agent says arrives twice — once as its final
// message and again as the run's result — and printing the same three
// paragraphs back to back reads like a stutter, or like something ran twice.
func reporter(onProgress func(Progress)) func(kind, text string) {
	last := ""
	return func(kind, text string) {
		text = strings.TrimSpace(text)
		if onProgress == nil || text == "" {
			return
		}
		if kind == "done" && text == last {
			onProgress(Progress{Kind: "done"})
			return
		}
		last = text
		onProgress(Progress{Kind: kind, Text: text})
	}
}

// readEvent turns one line of Claude Code's stream into something worth saying.
//
// Most of the stream is not: hook callbacks, rate-limit notices, the tool
// results themselves. What a person wants is the agent's own sentences and the
// name of what it is doing, which is two of the eight event types.
func readEvent(line []byte) (Progress, bool) {
	line = trimSpace(line)
	if len(line) == 0 {
		return Progress{}, false
	}
	var ev struct {
		Type    string `json:"type"`
		Subtype string `json:"subtype"`
		Message struct {
			Content []struct {
				Type  string          `json:"type"`
				Text  string          `json:"text"`
				Name  string          `json:"name"`
				Input json.RawMessage `json:"input"`
			} `json:"content"`
		} `json:"message"`
		IsError bool   `json:"is_error"`
		Result  string `json:"result"`
	}
	if err := json.Unmarshal(line, &ev); err != nil {
		return Progress{}, false
	}

	switch ev.Type {
	case "assistant":
		for _, c := range ev.Message.Content {
			switch c.Type {
			case "text":
				if text := strings.TrimSpace(c.Text); text != "" {
					return Progress{Kind: kindOf(text), Text: text}, true
				}
			case "tool_use":
				if text := doing(c.Name, c.Input); text != "" {
					return Progress{Kind: "doing", Text: text}, true
				}
			}
		}
	case "result":
		if ev.IsError {
			text := strings.TrimSpace(ev.Result)
			if text == "" {
				text = "The assistant stopped before it finished."
			}
			return Progress{Kind: "failed", Text: text}, true
		}
		return Progress{Kind: "done", Text: strings.TrimSpace(ev.Result)}, true
	}
	return Progress{}, false
}

// kindOf spots the sentences that are asking the person for something.
//
// An agent that has opened a consent screen and is waiting has said so in
// words; lifting that out of the log is what turns a scrolling transcript into
// a screen that tells you it is your turn.
func kindOf(text string) string {
	low := strings.ToLower(text)
	for _, marker := range []string{
		"your turn", "over to you", "please click", "click allow", "click continue",
		"sign in", "signed in?", "waiting for you", "i need you to", "go ahead and",
	} {
		if strings.Contains(low, marker) {
			return "needs-you"
		}
	}
	return "says"
}

// doing names a tool call in a person's words.
//
// Deliberately vague about arguments. A browser snapshot's input is a page full
// of somebody's email; the useful thing to show is "Reading the page", not its
// contents.
func doing(name string, input json.RawMessage) string {
	switch {
	case name == "Bash":
		var in struct {
			Description string `json:"description"`
			Command     string `json:"command"`
		}
		_ = json.Unmarshal(input, &in)
		if d := strings.TrimSpace(in.Description); d != "" {
			return d
		}
		if c := strings.TrimSpace(in.Command); c != "" {
			return "Running " + firstWord(c)
		}
		return "Running a command"
	case strings.Contains(name, "navigate"):
		var in struct {
			URL string `json:"url"`
		}
		_ = json.Unmarshal(input, &in)
		if host := hostOf(in.URL); host != "" {
			return "Opening " + host
		}
		return "Opening a page"
	case strings.Contains(name, "click"):
		var in struct {
			Element string `json:"element"`
		}
		_ = json.Unmarshal(input, &in)
		if e := strings.TrimSpace(in.Element); e != "" {
			return "Clicking " + e
		}
		return "Clicking"
	case strings.Contains(name, "type") || strings.Contains(name, "fill"):
		return "Filling in the form"
	case strings.Contains(name, "snapshot") || strings.Contains(name, "screenshot"):
		return "Reading the page"
	case strings.Contains(name, "wait"):
		return "Waiting for the page"
	case name == "Read" || name == "Write" || name == "Edit":
		return ""
	}
	return ""
}

func firstWord(s string) string {
	if i := strings.IndexAny(s, " \t\n"); i > 0 {
		return s[:i]
	}
	return s
}

func hostOf(raw string) string {
	i := strings.Index(raw, "://")
	if i < 0 {
		return ""
	}
	rest := raw[i+3:]
	if j := strings.IndexAny(rest, "/?#"); j >= 0 {
		rest = rest[:j]
	}
	return rest
}

func trimSpace(b []byte) []byte {
	return []byte(strings.TrimSpace(string(b)))
}
