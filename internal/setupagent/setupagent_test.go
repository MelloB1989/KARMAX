package setupagent

import (
	"strings"
	"testing"
)

func read(t *testing.T, line string) Progress {
	t.Helper()
	p, ok := readEvent([]byte(line))
	if !ok {
		t.Fatalf("nothing read from %s", line)
	}
	return p
}

// The agent's own sentences are the progress bar, so they have to survive.
func TestAssistantTextIsReported(t *testing.T) {
	p := read(t, `{"type":"assistant","message":{"content":[{"type":"text","text":"Creating the project now."}]}}`)
	if p.Kind != "says" || p.Text != "Creating the project now." {
		t.Fatalf("got %+v", p)
	}
}

// "Your turn" is the one line somebody must not scroll past.
//
// An agent waiting at a consent screen has said so in words. Without lifting it
// out, a person watching a transcript sees the log stop and assumes it broke.
func TestHandoverIsMarked(t *testing.T) {
	for _, text := range []string{
		"Your turn: click Allow on the consent screen.",
		"Please click Continue, then Allow.",
		"I need you to sign in with the account you want connected.",
	} {
		p := read(t, `{"type":"assistant","message":{"content":[{"type":"text","text":`+quote(text)+`}]}}`)
		if p.Kind != "needs-you" {
			t.Errorf("%q came back as %q, not a handover", text, p.Kind)
		}
	}
	ordinary := "Enabled the Calendar API."
	p := read(t, `{"type":"assistant","message":{"content":[{"type":"text","text":`+quote(ordinary)+`}]}}`)
	if p.Kind == "needs-you" {
		t.Errorf("%q was mistaken for a handover", ordinary)
	}
}

// Tool calls are named in a person's words, never in their arguments.
func TestToolCallsAreDescribedNotDumped(t *testing.T) {
	cases := []struct{ line, want string }{
		{`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"mcp__playwright__browser_navigate","input":{"url":"https://console.cloud.google.com/apis"}}]}}`,
			"Opening console.cloud.google.com"},
		{`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"mcp__playwright__browser_click","input":{"element":"Create credentials button"}}]}}`,
			"Clicking Create credentials button"},
		{`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"mcp__playwright__browser_snapshot","input":{}}]}}`,
			"Reading the page"},
		{`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Bash","input":{"command":"gog auth list --json","description":"Checking which Google accounts are connected"}}]}}`,
			"Checking which Google accounts are connected"},
	}
	for _, c := range cases {
		p := read(t, c.line)
		if p.Kind != "doing" || p.Text != c.want {
			t.Errorf("got %+v, want %q", p, c.want)
		}
	}
}

// A page snapshot's input is a page full of somebody's email. The description
// must not carry it.
func TestPageContentsNeverReachTheProgressLine(t *testing.T) {
	line := `{"type":"assistant","message":{"content":[{"type":"tool_use","name":"mcp__playwright__browser_snapshot","input":{"raw":"inbox: dinner with mum, invoice from acme"}}]}}`
	p := read(t, line)
	if strings.Contains(p.Text, "dinner") || strings.Contains(p.Text, "invoice") {
		t.Fatalf("the page leaked into the progress line: %q", p.Text)
	}
}

// A shell command with no description still says something, without printing
// the whole command line.
func TestBashWithoutADescriptionIsStillReadable(t *testing.T) {
	p := read(t, `{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Bash","input":{"command":"gog auth credentials set /home/x/Downloads/client_secret_9182.json"}}]}}`)
	if p.Text != "Running gog" {
		t.Fatalf("got %q", p.Text)
	}
	if strings.Contains(p.Text, "client_secret") {
		t.Fatal("a credential path reached the progress line")
	}
}

// The end of the run is reported either way.
func TestResultsAreReported(t *testing.T) {
	ok := read(t, `{"type":"result","is_error":false,"result":"Google is connected for you@example.com."}`)
	if ok.Kind != "done" {
		t.Fatalf("got %+v", ok)
	}
	bad := read(t, `{"type":"result","is_error":true,"result":"Stopped: billing was required."}`)
	if bad.Kind != "failed" || !strings.Contains(bad.Text, "billing") {
		t.Fatalf("got %+v", bad)
	}
}

// Everything else in the stream is noise, and showing it would bury the parts
// that matter.
func TestNoiseIsDropped(t *testing.T) {
	for _, line := range []string{
		`{"type":"system","subtype":"init","tools":["Bash"],"model":"claude-opus-5"}`,
		`{"type":"rate_limit_event","rate_limit_info":{}}`,
		`{"type":"user","message":{"content":[{"type":"tool_result","content":"hi"}]}}`,
		`not json at all`,
		``,
		`   `,
	} {
		if p, ok := readEvent([]byte(line)); ok {
			t.Errorf("%s produced %+v", line, p)
		}
	}
}

// Every recipe is complete enough to run.
func TestRecipesAreWellFormed(t *testing.T) {
	for id, r := range Recipes() {
		if r.ID != id {
			t.Errorf("%s is filed under the wrong id", r.ID)
		}
		if strings.TrimSpace(r.Name) == "" || strings.TrimSpace(r.Lede) == "" {
			t.Errorf("%s has nothing to say while it starts", id)
		}
		if r.Prompt == nil {
			t.Fatalf("%s has no prompt", id)
		}
		prompt := r.Prompt(Env{Bin: map[string]string{"gog": "/usr/local/bin/gog"}, Account: "you@example.com"})
		if len(prompt) < 200 {
			t.Errorf("%s's prompt is too thin to follow", id)
		}
		// The handover rule is what stops an agent typing somebody's password.
		if !strings.Contains(prompt, "Your turn:") {
			t.Errorf("%s never tells the agent how to hand back", id)
		}
	}
}

// A recipe that names a binary must interpolate the resolved path, not the
// bare name — the daemon's PATH is not the operator's shell's.
func TestGooglesPromptUsesTheResolvedBinary(t *testing.T) {
	r, _ := Lookup("google")
	prompt := r.Prompt(Env{Bin: map[string]string{"gog": "/opt/tools/gog"}, Account: "you@example.com"})
	if !strings.Contains(prompt, "/opt/tools/gog auth list") {
		t.Fatal("the prompt does not use the resolved gog path")
	}
	if !strings.Contains(prompt, "you@example.com") {
		t.Fatal("the prompt does not name the account to connect")
	}
}

func quote(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `\"`) + `"`
}

// The closing summary arrives twice — as the agent's last message and again as
// the run's result — and printing it twice reads like something ran twice.
func TestTheSummaryIsNotSaidTwice(t *testing.T) {
	var got []Progress
	say := reporter(func(p Progress) { got = append(got, p) })

	say("says", "Google is connected for you@example.com.")
	say("done", "Google is connected for you@example.com.")

	if len(got) != 2 {
		t.Fatalf("got %d messages, want 2", len(got))
	}
	if got[1].Kind != "done" || got[1].Text != "" {
		t.Fatalf("the repeat was not swallowed: %+v", got[1])
	}
}

// A closing line that says something new is still said.
func TestANewSummaryIsKept(t *testing.T) {
	var got []Progress
	say := reporter(func(p Progress) { got = append(got, p) })

	say("says", "Enabled the Calendar API.")
	say("done", "Google is connected for you@example.com.")

	if len(got) != 2 || got[1].Text == "" {
		t.Fatalf("the closing line was lost: %+v", got)
	}
}

// Nothing empty reaches the person, whatever the agent emitted.
func TestEmptyProgressIsDropped(t *testing.T) {
	var got []Progress
	say := reporter(func(p Progress) { got = append(got, p) })
	say("says", "")
	say("doing", "   ")
	if len(got) != 0 {
		t.Fatalf("empty messages got through: %+v", got)
	}
}
