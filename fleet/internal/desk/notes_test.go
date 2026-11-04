package desk

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/MelloB1989/karmax/fleet/internal/fleetapi"
)

func TestParseNote(t *testing.T) {
	done := parseNote([]byte("stop 1767225600\n"), nil)
	if done == nil || done.Kind != fleetapi.NoteDone || !done.At.Equal(time.Unix(1767225600, 0)) {
		t.Fatalf("stop: %+v", done)
	}
	needs := parseNote([]byte("notification 1767225660\n"+`{"session_id":"x","message":"Claude needs your permission to use Bash","notification_type":"permission_prompt"}`), done)
	if needs == nil || needs.Kind != fleetapi.NoteNeeds || needs.Text != "needs your permission to use Bash" {
		t.Fatalf("permission: %+v", needs)
	}
	// Claude Code's reminder a minute after a turn ended is not news.
	if got := parseNote([]byte("notification 1767225700\n"+`{"message":"Claude is waiting for your input","notification_type":"idle_prompt"}`), done); got != done {
		t.Fatalf("idle reminder after a turn: %+v", got)
	}
	if got := parseNote([]byte("notification 1767225700\n"+`{"message":"Claude is waiting for your input","notification_type":"idle_prompt"}`), needs); got == needs || got.Text != "is waiting for your input" {
		t.Fatalf("idle reminder after a question: %+v", got)
	}
	for _, b := range []string{"prompt 1767225800\n", "", "nonsense", "stop notanumber\n"} {
		if got := parseNote([]byte(b), needs); got != nil {
			t.Errorf("%q: %+v, want none", b, got)
		}
	}
}

// fleetd writes Claude Code's hooks; a hook run as Claude Code runs it
// leaves the session's note, and a prompt clears it, keeping nothing of it.
func TestClaudeCodesHooksLeaveTheSessionsNote(t *testing.T) {
	d := newTestDesk(t)
	state := filepath.Dir(d.State)
	d.HookScript, d.HookSettings = filepath.Join(state, "desk-hook.sh"), filepath.Join(state, "desk-hooks.json")
	s := d.create(fleetapi.DeskCreate{Dir: "~/code", Name: "api"})
	eventually(t, "claude's start", func() bool { return len(d.starts()) == 1 })
	if got := d.starts()[0]; !strings.HasSuffix(got, "--settings "+d.HookSettings) {
		t.Fatalf("claude started with %q, want its hooks", got)
	}
	if env := d.tmux("show-environment", "-t", "="+s.Name, "FLEET_DESK_SESSION"); env != "FLEET_DESK_SESSION=api" {
		t.Fatalf("the session's environment: %q", env)
	}
	var settings struct {
		Hooks map[string][]struct {
			Hooks []struct{ Type, Command string }
		}
	}
	b, _ := os.ReadFile(d.HookSettings)
	if err := json.Unmarshal(b, &settings); err != nil {
		t.Fatalf("settings %s: %v", b, err)
	}
	run := func(event, payload string) {
		t.Helper()
		var cmd string
		for name, hs := range settings.Hooks {
			if strings.EqualFold(name, map[string]string{"notification": "Notification", "stop": "Stop", "prompt": "UserPromptSubmit"}[event]) {
				cmd = hs[0].Hooks[0].Command
			}
		}
		if cmd == "" {
			t.Fatalf("no hook for %s in %s", event, b)
		}
		c := exec.Command("sh", "-c", cmd)
		c.Env = append(os.Environ(), "FLEET_DESK_SESSION=api", "FLEET_DESK_NOTES="+d.notesDir())
		c.Stdin = strings.NewReader(payload)
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("%s hook: %v %s", event, err, out)
		}
	}
	note := func() *fleetapi.DeskNote {
		for _, x := range d.list(0) {
			if x.Name == "api" {
				return x.Note
			}
		}
		return nil
	}
	run("notification", `{"message":"Claude needs your permission to use Bash","notification_type":"permission_prompt"}`)
	if n := note(); n == nil || n.Kind != fleetapi.NoteNeeds || n.Text != "needs your permission to use Bash" {
		t.Fatalf("after a notification: %+v", n)
	}
	time.Sleep(1100 * time.Millisecond) // a new second: the file's time changes
	run("prompt", `{"prompt":"a secret plan"}`)
	if n := note(); n != nil {
		t.Fatalf("after a prompt: %+v", n)
	}
	if b, _ := os.ReadFile(filepath.Join(d.notesDir(), "api")); strings.Contains(string(b), "secret") {
		t.Fatalf("the prompt was kept: %q", b)
	}
	run("stop", `{"stop_hook_active":false}`)
	if n := note(); n == nil || n.Kind != fleetapi.NoteDone {
		t.Fatalf("after a turn: %+v", n)
	}
	if err := d.End(bg, "api"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(d.notesDir(), "api")); !os.IsNotExist(err) {
		t.Fatal("an ended session's note stays")
	}
}
