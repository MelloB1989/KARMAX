package agent

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

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

	wts, err := l.a.Worktrees(ctx)
	if err != nil || len(wts) != 2 {
		t.Fatalf("worktrees %+v, %v", wts, err)
	}
	byPath := map[string]Worktree{wts[0].Path: wts[0], wts[1].Path: wts[1]}
	if !byPath[clean].Safe() || byPath[dirty].Safe() || byPath[dirty].Unpushed != 1 || byPath[dirty].Dirty != 1 {
		t.Fatalf("worktrees = %+v", wts)
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
