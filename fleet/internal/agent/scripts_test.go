package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/MelloB1989/karmax/fleet/internal/reconcile"
	"github.com/MelloB1989/karmax/fleet/internal/target"
)

// The container-side scripts, run for real: a fake `docker` runs the command
// locally, HOME and FLEET_WORK point at a temp dir, and tmux runs on a
// private socket. What this cannot reach is the image itself (spike, phase 2).

type local struct {
	t    *testing.T
	home string
	work string
	a    *Agent
	bin  string
}

func newLocal(t *testing.T) *local {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
	root := t.TempDir()
	l := &local{t: t, home: filepath.Join(root, "home"), work: filepath.Join(root, "work"), bin: filepath.Join(root, "bin")}
	for _, d := range []string{l.home + "/.claude/sessions", l.home + "/.claude/fleet", l.home + "/.claude/projects", l.work, l.bin} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write := func(p, body string) {
		if err := os.WriteFile(p, []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// docker: inspect says running; exec drops its flags and container name.
	write(l.bin+"/docker", `#!/bin/bash
case "$1" in
  inspect) echo true ;;
  exec) shift; while [[ "$1" == -* ]]; do shift; done; shift; exec "$@" ;;
esac
`)
	// claude agents --json: every session file in the registry, as a row.
	write(l.bin+"/claude", `#!/bin/bash
printf '['; sep=
for f in "$HOME"/.claude/sessions/*.json; do [ -e "$f" ] || continue; printf '%s' "$sep"; cat "$f"; sep=,; done
printf ']\n'
`)
	t.Setenv("PATH", l.bin+":"+os.Getenv("PATH"))
	t.Setenv("HOME", l.home)
	t.Setenv("FLEET_WORK", l.work)
	t.Setenv("TMUX_TMPDIR", root)
	l.a = &Agent{Name: "agent-03", C: target.Container{Exec: []string{l.bin + "/docker"}, Name: "agent-03"}}
	t.Cleanup(func() { _ = exec.Command("tmux", "kill-server").Run() })
	return l
}

// startPane runs a long sleep as the pane, carrying env, and registers it in
// the session registry under name.
func (l *local) startPane(name string, env ...string) int {
	l.t.Helper()
	args := []string{"new-session", "-d", "-s", "main"}
	for _, e := range env {
		args = append(args, "-e", e)
	}
	args = append(args, "exec sleep 300")
	if out, err := exec.Command("tmux", args...).CombinedOutput(); err != nil {
		l.t.Fatalf("tmux: %v %s", err, out)
	}
	out, err := exec.Command("tmux", "list-panes", "-t", "main", "-F", "#{pane_pid}").Output()
	if err != nil {
		l.t.Fatal(err)
	}
	var pid int
	fmt.Sscan(string(out), &pid)
	time.Sleep(100 * time.Millisecond) // let the exec land so environ is the sleep's
	l.register(pid, name, "s1")
	return pid
}

func (l *local) register(pid int, name, sid string) {
	l.t.Helper()
	body := fmt.Sprintf(`{"pid":%d,"kind":"interactive","sessionId":%q,"name":%q,"status":"idle"}`, pid, sid, name)
	if err := os.WriteFile(fmt.Sprintf("%s/.claude/sessions/%d.json", l.home, pid), []byte(body), 0o644); err != nil {
		l.t.Fatal(err)
	}
}

func (l *local) file(rel, body string) {
	l.t.Helper()
	p := filepath.Join(l.home, rel)
	_ = os.MkdirAll(filepath.Dir(p), 0o755)
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		l.t.Fatal(err)
	}
}

func TestSnapshotScriptForReal(t *testing.T) {
	l := newLocal(t)
	pid := l.startPane("agent-03", "FLEET_AGENT=agent-03")
	// Another agent's session in the shared registry: a process of ours with
	// a different FLEET_AGENT.
	other := exec.Command("sleep", "300")
	other.Env = append(os.Environ(), "FLEET_AGENT=agent-01")
	if err := other.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = other.Process.Kill(); _ = other.Wait() })
	l.register(other.Process.Pid, "agent-01", "s5")

	l.file(".claude/projects/-work/s1.jsonl", strings.Repeat("x", 1234))
	l.file(".claude/fleet/status.json", `{"session_id":"s1","context_window":{"used_percentage":7}}`)
	l.file(".claude/fleet/events.jsonl", `{"t":1,"event":"Stop","session":"s1"}`+"\n")

	o, err := l.a.Observe(context.Background(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if !o.ContainerUp || !o.PaneAlive || o.PanePID != pid {
		t.Fatalf("pane: up=%v alive=%v pid=%d want %d", o.ContainerUp, o.PaneAlive, o.PanePID, pid)
	}
	if len(o.Sessions) != 1 || o.Sessions[0].SessionID != "s1" {
		t.Fatalf("sessions = %+v: want only this agent's, told apart by FLEET_AGENT", o.Sessions)
	}
	if o.TranscriptSize != 1234 || o.Status == nil || o.Status.ContextPct() != 7 || len(o.Events) != 1 {
		t.Fatalf("obs = size %d status %+v events %d", o.TranscriptSize, o.Status, len(o.Events))
	}

	// Unacknowledged events come back; acknowledged ones do not, and new ones
	// written meanwhile are kept.
	o, _ = l.a.Observe(context.Background(), time.Now())
	if len(o.Events) != 1 {
		t.Fatalf("unacked events = %d, want the same 1 again", len(o.Events))
	}
	if err := l.a.AckEvents(context.Background()); err != nil {
		t.Fatal(err)
	}
	l.file(".claude/fleet/events.jsonl", `{"t":2,"event":"SessionStart","session":"s1"}`+"\n")
	o, _ = l.a.Observe(context.Background(), time.Now())
	if len(o.Events) != 1 || o.Events[0].Event != "SessionStart" {
		t.Fatalf("after ack: %+v", o.Events)
	}
}

func TestNextScriptForReal(t *testing.T) {
	l := newLocal(t)
	l.startPane("agent-03")
	ctx := context.Background()
	next := filepath.Join(l.work, ".fleet", "next")

	if err := l.a.Restart(ctx, "s9"); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(next); err != nil || string(b) != "s9" {
		t.Fatalf("next = %q, %v", b, err)
	}
	if exec.Command("tmux", "has-session", "-t", "main").Run() == nil {
		t.Fatal("the pane was not killed")
	}
	if err := l.a.Fresh(ctx); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(next); err != nil || len(b) != 0 {
		t.Fatalf("fresh: next = %q, %v", b, err)
	}
	// A restart that names no session must not undo a rotation agent-run has
	// not picked up yet: "fresh" stays pending.
	if err := l.a.Restart(ctx, ""); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(next); err != nil || len(b) != 0 {
		t.Fatalf("continue clobbered a pending fresh start: next = %q, %v", b, err)
	}
}

func TestTranscriptScriptsForReal(t *testing.T) {
	l := newLocal(t)
	ctx := context.Background()
	l.file(".claude/projects/-work/s1.jsonl", "{\"a\":1}\n")
	l.file(".claude/projects/-work/s1/subagents/agent-x.jsonl", "{\"b\":2}\n")
	l.file(".claude/projects/-work/other.jsonl", "{}\n")

	tr, err := l.a.PullTranscript(ctx, "s1")
	if err != nil {
		t.Fatal(err)
	}
	if tr.Project != "-work" || string(tr.Main) != "{\"a\":1}\n" || string(tr.Sub["subagents/agent-x.jsonl"]) != "{\"b\":2}\n" {
		t.Fatalf("pulled %+v", tr)
	}
	h, err := l.a.HashTranscript(ctx, "s1")
	if err != nil || h != "e346432021b04179518d9614f3560ccd71354a4ee101ddcb893d6959a9d6301c" {
		t.Fatalf("hash = %q, %v", h, err)
	}
	if err := l.a.DeleteTranscript(ctx, "s1"); err != nil {
		t.Fatal(err)
	}
	if _, err := l.a.PullTranscript(ctx, "s1"); err == nil {
		t.Fatal("transcript still there after delete")
	}
	if _, err := os.Stat(filepath.Join(l.home, ".claude/projects/-work/other.jsonl")); err != nil {
		t.Fatal("delete touched another session's transcript")
	}
	if err := l.a.PushTranscript(ctx, "s1", tr); err != nil {
		t.Fatal(err)
	}
	back, err := l.a.PullTranscript(ctx, "s1")
	if err != nil || string(back.Main) != string(tr.Main) || len(back.Sub) != 1 {
		t.Fatalf("restored %+v, %v", back, err)
	}
}

func TestWorktreesScriptForReal(t *testing.T) {
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq not installed (it is in the image)")
	}
	l := newLocal(t)
	ctx := context.Background()
	git := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	origin := filepath.Join(t.TempDir(), "origin.git")
	repo := filepath.Join(t.TempDir(), "repo")
	git(".", "init", "-q", "--bare", origin)
	git(".", "init", "-q", repo)
	git(repo, "commit", "-q", "--allow-empty", "-m", "base")
	git(repo, "remote", "add", "origin", origin)
	git(repo, "push", "-q", "origin", "HEAD:refs/heads/main")
	clean, dirty := filepath.Join(l.work, "wt", "clean"), filepath.Join(l.work, "wt", "dirty")
	git(repo, "worktree", "add", "-q", "--lock", "-b", "fleet/agent-03/clean", clean)
	git(clean, "push", "-q", "-u", "origin", "fleet/agent-03/clean")
	git(repo, "worktree", "add", "-q", "--lock", "-b", "fleet/agent-03/dirty", dirty)
	git(dirty, "commit", "-q", "--allow-empty", "-m", "local only")
	_ = os.WriteFile(filepath.Join(dirty, "wip.txt"), []byte("x"), 0o644)
	// How agents really leave them: pushed without -u (no upstream), and made
	// from origin/main, which becomes the upstream, then pushed to their own
	// branch. Both are on the remote: removing them loses nothing.
	git(repo, "fetch", "-q", "origin")
	noU, mainUp, notPushed := filepath.Join(l.work, "wt", "no-u"), filepath.Join(l.work, "wt", "main-up"), filepath.Join(l.work, "wt", "not-pushed")
	git(repo, "worktree", "add", "-q", "--lock", "-b", "fleet/agent-03/no-u", noU)
	git(noU, "commit", "-q", "--allow-empty", "-m", "work")
	git(noU, "push", "-q", "origin", "fleet/agent-03/no-u")
	git(repo, "worktree", "add", "-q", "--lock", "--track", "-b", "fleet/agent-03/main-up", mainUp, "origin/main")
	git(mainUp, "commit", "-q", "--allow-empty", "-m", "work")
	git(mainUp, "push", "-q", "origin", "fleet/agent-03/main-up")
	git(repo, "worktree", "add", "-q", "--lock", "--track", "-b", "fleet/agent-03/not-pushed", notPushed, "origin/main")
	git(notPushed, "commit", "-q", "--allow-empty", "-m", "local only")
	// One git cannot read at all: not taken for clean and pushed.
	broken := filepath.Join(l.work, "wt", "broken")
	_ = os.MkdirAll(broken, 0o755)
	_ = os.WriteFile(filepath.Join(broken, ".git"), []byte("gitdir: /nonexistent/x\n"), 0o644)

	wts, err := l.a.Worktrees(ctx)
	if err != nil || len(wts) != 6 {
		t.Fatalf("worktrees %+v, %v", wts, err)
	}
	byPath := map[string]Worktree{}
	for _, w := range wts {
		byPath[w.Path] = w
	}
	if !byPath[clean].Safe() || byPath[dirty].Safe() || byPath[dirty].Unpushed != 1 || byPath[dirty].Dirty != 1 {
		t.Fatalf("worktrees = %+v", wts)
	}
	if !byPath[noU].Safe() || !byPath[mainUp].Safe() || byPath[mainUp].Upstream != "origin/main" {
		t.Fatalf("pushed, yet kept: %+v %+v", byPath[noU], byPath[mainUp])
	}
	if w := byPath[notPushed]; w.Safe() || w.Unpushed != 1 {
		t.Fatalf("a commit on no remote: %+v", w)
	}
	if w := byPath[broken]; w.Safe() || w.Dirty != -1 || w.Unpushed != -1 {
		t.Fatalf("unreadable: %+v", w)
	}
	if err := l.a.RemoveWorktree(ctx, byPath[dirty]); err == nil {
		t.Fatal("removed a worktree with unpushed work")
	}
	if err := l.a.RemoveWorktree(ctx, byPath[clean]); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(clean); !os.IsNotExist(err) {
		t.Fatal("the clean, pushed worktree is still there")
	}
}

func TestEventScriptRecordsPreToolUse(t *testing.T) {
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq not installed")
	}
	home := t.TempDir()
	if err := os.MkdirAll(home+"/.claude/fleet", 0o755); err != nil {
		t.Fatal(err)
	}
	run := func(payload string) {
		t.Helper()
		cmd := exec.Command("sh", "../../../packaging/fleet/home/event.sh")
		cmd.Env = append(os.Environ(), "HOME="+home)
		cmd.Stdin = strings.NewReader(payload)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("event.sh: %v %s", err, out)
		}
	}
	run(`{"hook_event_name":"PreToolUse","session_id":"s1","tool_name":"Bash","tool_input":{"command":"secret"}}`)
	run(`{"hook_event_name":"PostToolUse","session_id":"s1","tool_name":"Bash","tool_response":{}}`)
	b, err := os.ReadFile(home + "/.claude/fleet/events.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) != 2 {
		t.Fatalf("lines = %q", lines)
	}
	pre, post := lines[0], lines[1]
	for _, want := range []string{`"event":"PreToolUse"`, `"session":"s1"`, `"tool":"Bash"`} {
		if !strings.Contains(pre, want) {
			t.Errorf("pre %s lacks %s", pre, want)
		}
	}
	if strings.Contains(pre, `"ok"`) || strings.Contains(pre, "secret") {
		t.Errorf("pre %s must carry no ok and none of the tool's input", pre)
	}
	if !strings.Contains(post, `"ok":true`) {
		t.Errorf("post %s lost its ok", post)
	}
}

func TestTmuxConfPinsSizeAndHasNoPrefix(t *testing.T) {
	for _, bin := range []string{"tmux", "script"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skip(bin + " not installed")
		}
	}
	root := t.TempDir()
	conf, _ := filepath.Abs("../../../packaging/fleet/tmux.conf")
	sock := root + "/sock"
	tm := func(args ...string) string {
		t.Helper()
		out, err := exec.Command("tmux", append([]string{"-f", conf, "-S", sock}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("tmux %v: %v %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	t.Cleanup(func() { _ = exec.Command("tmux", "-S", sock, "kill-server").Run() })
	waitFor := func(what string, ok func() bool) {
		t.Helper()
		for i := 0; i < 100; i++ {
			if ok() {
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
		t.Fatalf("timed out waiting for %s", what)
	}
	size := func() string { return tm("display-message", "-p", "-t", "main", "#{window_width}x#{window_height}") }

	// The pane records every byte it is typed, visibly.
	pane := root + "/pane.out"
	tm("new-session", "-d", "-s", "main", "-x", "220", "-y", "50", "exec cat -v > "+pane)
	if got := size(); got != "220x50" {
		t.Fatalf("new session = %s, want 220x50", got)
	}

	// A 80x24 client attaches: the window must not follow it.
	client := exec.Command("script", "-qec", "stty cols 80 rows 24; exec tmux -f "+conf+" -S "+sock+" attach -t main", "/dev/null")
	in, err := client.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan error, 1)
	go func() { exited <- client.Wait() }()
	t.Cleanup(func() { _ = client.Process.Kill() })
	waitFor("the client to attach", func() bool { return tm("list-clients") != "" })
	if got := size(); got != "220x50" {
		t.Errorf("with an 80x24 client attached the window is %s, want 220x50", got)
	}
	if got := tm("show-options", "-gv", "window-size"); got != "manual" {
		t.Errorf("window-size = %q", got)
	}

	// C-b is not a prefix: it, and the key after it, reach the pane.
	_, _ = in.Write([]byte("\x02x\n"))
	waitFor("C-b to reach the pane", func() bool {
		b, _ := os.ReadFile(pane)
		return strings.Contains(string(b), "^Bx")
	})
	if tm("list-sessions") == "" {
		t.Fatal("session gone")
	}

	// C-] detaches the client and leaves the session running.
	_, _ = in.Write([]byte{0x1d})
	select {
	case <-exited:
	case <-time.After(5 * time.Second):
		t.Fatal("C-] did not detach the client")
	}
	if got := tm("list-sessions"); !strings.HasPrefix(got, "main:") {
		t.Errorf("after detach: %q", got)
	}
	// No key is bound but C-]. (tmux keeps its mouse defaults after
	// unbind-key -a; they are inert while the mouse option is off.)
	var bound []string
	for _, l := range strings.Split(tm("list-keys"), "\n") {
		if f := strings.Fields(l); len(f) > 3 && !isMouseKey(f[3]) {
			bound = append(bound, f[3]+" "+f[4])
		}
	}
	if len(bound) != 1 || bound[0] != "C-] detach-client" {
		t.Errorf("bound keys = %q, want only C-] detach-client", bound)
	}
	if got := tm("show-options", "-gv", "mouse"); got != "off" {
		t.Errorf("mouse = %q, want off", got)
	}
}

func isMouseKey(k string) bool {
	for _, m := range []string{"Mouse", "Wheel", "Click", "Drag"} {
		if strings.Contains(k, m) {
			return true
		}
	}
	return false
}

// The snapshot carries the pane's text, so a dialog that blocks claude
// before it registers a session can be seen.
func TestSnapshotCarriesThePanesScreen(t *testing.T) {
	l := newLocal(t)
	text := "By proceeding, you accept all responsibility for actions taken while running in Bypass Permissions mode."
	if out, err := exec.Command("tmux", "new-session", "-d", "-s", "main", "-x", "200", "-y", "50",
		"sh", "-c", "printf '%s\\n' \"$0\"; exec sleep 300", text).CombinedOutput(); err != nil {
		t.Fatalf("tmux: %v %s", err, out)
	}
	var o reconcile.Obs
	for i := 0; i < 50 && !strings.Contains(o.Screen, "Bypass Permissions"); i++ {
		time.Sleep(100 * time.Millisecond)
		o, _ = l.a.Observe(context.Background(), time.Now())
	}
	if !strings.Contains(o.Screen, text) || reconcile.Dialog(o.Screen) == "" {
		t.Fatalf("screen = %q", o.Screen)
	}
}

// Restart must behave like recreate: Claude Code moves its bypass consent into
// the user settings and out of ~/.claude.json, and agent-run puts the shipped
// settings.json back on every start, so that file has to carry the consent.
func TestShippedSettingsSkipTheBypassDialogAndAgentRunReinstallsThem(t *testing.T) {
	raw, err := os.ReadFile("../../../packaging/fleet/home/settings.json")
	if err != nil {
		t.Fatal(err)
	}
	var s struct {
		Skip        bool `json:"skipDangerousModePermissionPrompt"`
		Permissions struct {
			DefaultMode string `json:"defaultMode"`
		} `json:"permissions"`
	}
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatal(err)
	}
	if !s.Skip || s.Permissions.DefaultMode != "bypassPermissions" {
		t.Fatalf("settings.json = %s: needs skipDangerousModePermissionPrompt true with bypassPermissions", raw)
	}
	run, _ := os.ReadFile("../../../packaging/fleet/agent-run")
	if !strings.Contains(string(run), `cp /opt/fleet/settings.json "$C/settings.json"`) {
		t.Fatal("agent-run no longer copies the shipped settings.json on every start")
	}
}

// With HOME unwritable agent-run must not start claude (into the first-run
// wizard): it logs agent-run.home_unwritable where fleetd reads it, and retries.
func TestAgentRunHoldsOffWhileHomeIsUnwritable(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root writes anywhere")
	}
	l := newLocal(t)
	// As in the broken image: no ~/.claude at all, and a HOME nobody can write.
	if err := os.RemoveAll(l.home + "/.claude"); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(l.home, 0o555); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(l.home, 0o755)
	cmd := exec.Command("sh", "../../../packaging/fleet/agent-run")
	code := t.TempDir() // a code dir with something in it, so that check passes
	if err := os.Mkdir(filepath.Join(code, "repo"), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd.Env = append(os.Environ(), "FLEET_AGENT=agent-03", "FLEET_HOME_RETRY=1", "FLEET_CODE="+code)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	log := filepath.Join(l.work, ".fleet", "events.jsonl")
	var lines []string
	for i := 0; i < 100 && len(lines) < 2; i++ { // two lines: it retries
		time.Sleep(100 * time.Millisecond)
		b, _ := os.ReadFile(log)
		lines = strings.Split(strings.TrimSpace(string(b)), "\n")
	}
	if len(lines) < 2 {
		t.Fatalf("no retried event in %s: %v", log, lines)
	}
	var e struct{ Event, Detail string }
	if err := json.Unmarshal([]byte(lines[0]), &e); err != nil || e.Event != "agent-run.home_unwritable" || !strings.Contains(e.Detail, l.home) {
		t.Fatalf("event = %s (%v)", lines[0], err)
	}
	if exec.Command("tmux", "has-session", "-t", "main").Run() == nil {
		t.Error("claude was started with HOME unwritable")
	}
	if _, err := os.Stat(filepath.Join(l.home, ".claude.json")); err == nil {
		t.Error("~/.claude.json written")
	}
}

// With the code directory empty (a bind of the NFS automount stub before the
// share was mounted) agent-run must not start claude and must not loop — the
// bind is fixed at container start — but log agent-run.code_missing and exit
// non-zero, so Docker's restart policy re-binds the mount.
func TestAgentRunExitsWhenTheCodeDirIsEmpty(t *testing.T) {
	l := newLocal(t)
	code := filepath.Join(t.TempDir(), "code")
	if err := os.MkdirAll(code, 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sh", "../../../packaging/fleet/agent-run")
	cmd.Env = append(os.Environ(), "FLEET_AGENT=agent-03", "FLEET_CODE="+code, "FLEET_CODE_RETRY=0")
	var stderr strings.Builder
	cmd.Stderr = &stderr
	done := make(chan error, 1)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		var ee *exec.ExitError
		if !errors.As(err, &ee) || ee.ExitCode() == 0 {
			t.Fatalf("agent-run exited with %v, want non-zero", err)
		}
	case <-time.After(10 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatal("agent-run did not exit: it must not loop on an empty code dir")
	}
	if !strings.Contains(stderr.String(), code) {
		t.Errorf("stderr = %q, want it to name %s", stderr.String(), code)
	}
	b, _ := os.ReadFile(filepath.Join(l.home, ".claude", "fleet", "events.jsonl"))
	var e struct{ Event, Detail string }
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(b))), &e); err != nil || e.Event != "agent-run.code_missing" || e.Detail != code {
		t.Fatalf("event = %s (%v)", b, err)
	}
	if exec.Command("tmux", "has-session", "-t", "main").Run() == nil {
		t.Error("claude was started on an empty code dir")
	}
	if _, err := os.Stat(filepath.Join(l.home, ".claude.json")); err == nil {
		t.Error("~/.claude.json written: agent-run went on past the check")
	}
}

// Agents' commits carry the operator's trailer only: Claude Code's own
// co-author line and PR footer are off.
func TestShippedSettingsTurnClaudeAttributionOff(t *testing.T) {
	raw, err := os.ReadFile("../../../packaging/fleet/home/settings.json")
	if err != nil {
		t.Fatal(err)
	}
	var s map[string]json.RawMessage
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatal(err)
	}
	if string(s["attribution"]) != "false" {
		t.Fatalf("settings.json attribution = %s, want false", s["attribution"])
	}
}
