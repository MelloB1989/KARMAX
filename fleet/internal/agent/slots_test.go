package agent

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/MelloB1989/karmax/fleet/internal/target"
)

// slot2 is agent-03's second slot, in the same container as l.a.
func (l *local) slot2() *Agent {
	return &Agent{Name: "agent-03:2", Slot: 2, C: target.Container{Exec: []string{l.bin + "/docker"}, Name: "agent-03"}}
}

func (l *local) workFile(rel, body string) {
	l.t.Helper()
	p := filepath.Join(l.work, rel)
	_ = os.MkdirAll(filepath.Dir(p), 0o755)
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		l.t.Fatal(err)
	}
}

func TestEachSlotSeesOnlyItsOwnPaneSessionStatusEventsAndOutbox(t *testing.T) {
	l := newLocal(t)
	p1 := l.startPaneOn("main", "agent-03", "s1", "FLEET_AGENT=agent-03")
	p2 := l.startPaneOn("main-2", "agent-03:2", "s2", "FLEET_AGENT=agent-03:2", "FLEET_SLOT=2")
	l.file(".claude/projects/-work/s1.jsonl", strings.Repeat("x", 100))
	l.file(".claude/projects/-work/s2.jsonl", strings.Repeat("x", 200))
	l.file(".claude/fleet/status.json", `{"session_id":"s1","context_window":{"used_percentage":10}}`)
	l.file(".claude/fleet/slot-2/status.json", `{"session_id":"s2","context_window":{"used_percentage":20}}`)
	l.file(".claude/fleet/events.jsonl", `{"t":1,"event":"Stop","session":"s1"}`+"\n")
	l.file(".claude/fleet/slot-2/events.jsonl", `{"t":1,"event":"UserPromptSubmit","session":"s2"}`+"\n"+`{"t":2,"event":"PreToolUse","session":"s2","tool":"Bash"}`+"\n")
	l.workFile(".fleet/current", "s1\n")
	l.workFile(".fleet/slot-2/current", "s2\n")
	l.workFile(".fleet/outbox/1-1.json", `{"from":"agent-03","to":"karmax","text":"one"}`)
	l.workFile(".fleet/slot-2/outbox/2-2.json", `{"from":"agent-03:2","to":"karmax","text":"two"}`)

	ctx := context.Background()
	o1, err := l.a.Observe(ctx, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	o2, err := l.slot2().Observe(ctx, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if o1.PanePID != p1 || o2.PanePID != p2 {
		t.Fatalf("panes: slot 1 %d (want %d), slot 2 %d (want %d)", o1.PanePID, p1, o2.PanePID, p2)
	}
	// Each slot's session is told apart by its process's FLEET_AGENT: a
	// sibling's session is not in the other's observation at all, so it can
	// never be taken for a stray there.
	if len(o1.Sessions) != 1 || o1.Sessions[0].SessionID != "s1" || len(o2.Sessions) != 1 || o2.Sessions[0].SessionID != "s2" {
		t.Fatalf("sessions: slot 1 %+v, slot 2 %+v", o1.Sessions, o2.Sessions)
	}
	if o1.CurrentSession != "s1" || o2.CurrentSession != "s2" || o1.TranscriptSize != 100 || o2.TranscriptSize != 200 {
		t.Fatalf("current %q/%q, transcripts %d/%d", o1.CurrentSession, o2.CurrentSession, o1.TranscriptSize, o2.TranscriptSize)
	}
	if o1.Status.ContextPct() != 10 || o2.Status.ContextPct() != 20 {
		t.Fatalf("status: slot 1 %v, slot 2 %v", o1.Status.ContextPct(), o2.Status.ContextPct())
	}
	if len(o1.Events) != 1 || o1.Events[0].Session != "s1" || len(o2.Events) != 2 || o2.Events[0].Session != "s2" {
		t.Fatalf("events: slot 1 %+v, slot 2 %+v", o1.Events, o2.Events)
	}
	if len(o1.Outbox) != 1 || o1.Outbox[0].Text != "one" || len(o2.Outbox) != 1 || o2.Outbox[0].Text != "two" {
		t.Fatalf("outbox: slot 1 %+v, slot 2 %+v", o1.Outbox, o2.Outbox)
	}

	// Acknowledging slot 2's events and outbox leaves slot 1's.
	if err := l.slot2().AckEvents(ctx); err != nil {
		t.Fatal(err)
	}
	if err := l.slot2().AckOutbox(ctx, []string{"2-2.json"}); err != nil {
		t.Fatal(err)
	}
	o1, _ = l.a.Observe(ctx, time.Now())
	o2, _ = l.slot2().Observe(ctx, time.Now())
	if len(o1.Events) != 1 || len(o1.Outbox) != 1 || len(o2.Events) != 0 || len(o2.Outbox) != 0 {
		t.Fatalf("after slot 2's acks: slot 1 events %d outbox %d, slot 2 events %d outbox %d",
			len(o1.Events), len(o1.Outbox), len(o2.Events), len(o2.Outbox))
	}
}

func TestRestartingSlot2LeavesSlot1Alone(t *testing.T) {
	l := newLocal(t)
	l.startPaneOn("main", "agent-03", "s1")
	l.startPaneOn("main-2", "agent-03:2", "s2")
	ctx := context.Background()

	if err := l.slot2().Restart(ctx, "s9"); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(l.work, ".fleet", "slot-2", "next")); err != nil || string(b) != "s9" {
		t.Fatalf("slot 2's next = %q, %v", b, err)
	}
	if _, err := os.Stat(filepath.Join(l.work, ".fleet", "next")); !os.IsNotExist(err) {
		t.Fatal("restarting slot 2 wrote slot 1's next")
	}
	if exec.Command("tmux", "has-session", "-t", "main-2").Run() == nil {
		t.Fatal("slot 2's pane was not killed")
	}
	if err := exec.Command("tmux", "has-session", "-t", "=main").Run(); err != nil {
		t.Fatal("restarting slot 2 killed slot 1's pane")
	}

	// Typing goes to the slot's own pane.
	l.startPaneOn("main-2", "agent-03:2", "s2")
	if _, err := exec.Command("tmux", "kill-session", "-t", "=main").CombinedOutput(); err != nil {
		t.Fatal(err)
	}
	if err := l.slot2().Interrupt(ctx); err != nil {
		t.Fatalf("slot 2 has its pane, yet: %v", err)
	}
	if err := l.a.Interrupt(ctx); err == nil {
		t.Fatal("slot 1's pane is gone, yet Interrupt typed somewhere")
	}
}

func TestASiblingSlotsWorktreeIsNeverSafeToRemove(t *testing.T) {
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq not installed (it is in the image)")
	}
	l := newLocal(t)
	ctx := context.Background()
	repo := filepath.Join(t.TempDir(), "repo")
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
	git(".", "init", "-q", "--bare", origin)
	git(".", "init", "-q", repo)
	git(repo, "commit", "-q", "--allow-empty", "-m", "base")
	git(repo, "remote", "add", "origin", origin)
	git(repo, "push", "-q", "origin", "HEAD:refs/heads/main")
	git(repo, "fetch", "-q", "origin")
	// Clean and pushed: by git alone, nothing to lose.
	mine, theirs := filepath.Join(l.work, "wt", "mine"), filepath.Join(l.work, "wt", "theirs")
	git(repo, "worktree", "add", "-q", "--detach", mine)
	git(repo, "worktree", "add", "-q", "--detach", theirs)
	// Slot 2's current session works in theirs; slot 1's in mine.
	l.workFile(".fleet/current", "s1\n")
	l.workFile(".fleet/slot-2/current", "s2\n")
	l.file(".claude/projects/-work/s1.jsonl", `{"cwd":"`+mine+`"}`+"\n")
	l.file(".claude/projects/-work/s2.jsonl", `{"tool_use":"cd `+theirs+` && go test"}`+"\n")

	wts, err := l.a.Worktrees(ctx)
	if err != nil || len(wts) != 2 {
		t.Fatalf("slot 1's worktrees %+v, %v", wts, err)
	}
	for _, w := range wts {
		switch w.Path {
		case mine:
			if !w.Safe() || w.UsedBySlot != 0 {
				t.Errorf("slot 1's own worktree: %+v", w)
			}
		case theirs:
			if w.Safe() || w.UsedBySlot != 2 {
				t.Errorf("slot 2's worktree, seen from slot 1: %+v (want used by slot 2, not safe)", w)
			}
			if err := l.a.RemoveWorktree(ctx, w); err == nil {
				t.Error("slot 1 removed the worktree slot 2 works in")
			}
		}
	}
	// And the other way round.
	wts, _ = l.slot2().Worktrees(ctx)
	for _, w := range wts {
		if w.Path == mine && w.UsedBySlot != 1 {
			t.Errorf("slot 1's worktree, seen from slot 2: %+v", w)
		}
		if w.Path == theirs && w.UsedBySlot != 0 {
			t.Errorf("slot 2's own worktree, seen from slot 2: %+v", w)
		}
	}
}

// agent-run starts FLEET_SLOTS supervising loops: each slot's claude in its
// own pane, named for its worker, with its own state directory in its env.
func TestAgentRunStartsOnePaneAndStateDirPerSlot(t *testing.T) {
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq not installed (it is in the image)")
	}
	l := newLocal(t)
	// claude, run as a slot's pane: writes down how it was started, and stays.
	if err := os.WriteFile(l.bin+"/claude", []byte(`#!/bin/bash
if [ "$1" = agents ]; then echo '[]'; exit 0; fi
mkdir -p "$HOME/started"
f="$HOME/started/${FLEET_SLOT:-none}"
{ echo "args=$*"; env | grep '^FLEET_' | sort; } > "$f.tmp" && mv "$f.tmp" "$f"
exec sleep 300
`), 0o755); err != nil {
		t.Fatal(err)
	}
	code := t.TempDir()
	if err := os.Mkdir(filepath.Join(code, "repo"), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sh", "../../../packaging/fleet/agent-run")
	cmd.Env = append(os.Environ(), "FLEET_AGENT=agent-03", "FLEET_NAME=agent-03", "FLEET_SLOTS=3", "FLEET_CODE="+code)
	// Its own process group, ended whole: a slot's loop that outlived the
	// test would go on starting panes once the temp dir is gone — on the
	// default tmux server, whoever's that is.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); _ = cmd.Wait() })

	started := func(slot string) map[string]string {
		b, err := os.ReadFile(filepath.Join(l.home, "started", slot))
		if err != nil {
			return nil
		}
		out := map[string]string{}
		for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
			k, v, _ := strings.Cut(line, "=")
			out[k] = v
		}
		return out
	}
	for i := 0; i < 100 && (started("1") == nil || started("2") == nil || started("3") == nil); i++ {
		time.Sleep(100 * time.Millisecond)
	}
	for k, want := range map[string]struct{ pane, name, dir string }{
		"1": {"main", "agent-03", filepath.Join(l.work, ".fleet")},
		"2": {"main-2", "agent-03:2", filepath.Join(l.work, ".fleet", "slot-2")},
		"3": {"main-3", "agent-03:3", filepath.Join(l.work, ".fleet", "slot-3")},
	} {
		got := started(k)
		if got == nil {
			t.Fatalf("slot %s never started its claude", k)
		}
		if !strings.Contains(got["args"], "--name "+want.name+" ") || got["FLEET_AGENT"] != want.name || got["FLEET_NAME"] != want.name ||
			got["FLEET_SLOT"] != k || got["FLEET_STATE"] != want.dir {
			t.Errorf("slot %s started as %v; want --name %s, FLEET_AGENT/FLEET_NAME %s, FLEET_SLOT %s, FLEET_STATE %s",
				k, got, want.name, want.name, k, want.dir)
		}
		if err := exec.Command("tmux", "has-session", "-t", "="+want.pane).Run(); err != nil {
			t.Errorf("slot %s: no pane %s", k, want.pane)
		}
		cur, err := os.ReadFile(filepath.Join(want.dir, "current"))
		if err != nil || !strings.Contains(got["args"], "--session-id "+strings.TrimSpace(string(cur))) {
			t.Errorf("slot %s: current %q (%v) is not the session it started: %s", k, cur, err, got["args"])
		}
	}

	// The slots run one supervising loop each: slot 2's pane, killed, comes
	// back on its own pending next; slot 1's pane stays the same process.
	pane1, _ := exec.Command("tmux", "list-panes", "-t", "=main:", "-F", "#{pane_pid}").Output()
	s2 := &Agent{Name: "agent-03:2", Slot: 2, C: l.a.C}
	if err := s2.Restart(context.Background(), "s-resumed"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 100 && !strings.Contains(started("2")["args"], "--resume s-resumed"); i++ {
		time.Sleep(100 * time.Millisecond)
	}
	if got := started("2")["args"]; !strings.Contains(got, "--resume s-resumed") {
		t.Fatalf("slot 2 restarted with %q, want --resume s-resumed", got)
	}
	if again, _ := exec.Command("tmux", "list-panes", "-t", "=main:", "-F", "#{pane_pid}").Output(); len(pane1) == 0 || string(again) != string(pane1) {
		t.Errorf("slot 1's pane went from %s to %s when slot 2 restarted", pane1, again)
	}
	// Every pane keeps the fixed size, the second session included.
	for _, p := range []string{"main", "main-2", "main-3"} {
		if out, _ := exec.Command("tmux", "display-message", "-p", "-t", "="+p+":", "#{window_width}x#{window_height}").Output(); strings.TrimSpace(string(out)) != "220x50" {
			t.Errorf("%s is %s, want 220x50", p, out)
		}
	}
}

// The hooks and the status line of a slot write where fleetd reads that
// slot: under ~/.claude/fleet/slot-k; slot 1 keeps today's files.
func TestHooksAndStatusLineWriteToTheirSlotsFiles(t *testing.T) {
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq not installed (it is in the image)")
	}
	home := t.TempDir()
	for _, d := range []string{"/.claude/fleet", "/.claude/fleet/slot-2"} {
		_ = os.MkdirAll(home+d, 0o755)
	}
	run := func(script, slot, in string) {
		t.Helper()
		cmd := exec.Command("sh", "../../../packaging/fleet/home/"+script)
		cmd.Env = append(os.Environ(), "HOME="+home, "FLEET_AGENT=agent-03:2")
		if slot != "" {
			cmd.Env = append(cmd.Env, "FLEET_SLOT="+slot)
		}
		cmd.Stdin = strings.NewReader(in)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%s: %v %s", script, err, out)
		}
	}
	run("event.sh", "2", `{"hook_event_name":"Stop","session_id":"s2"}`)
	run("event.sh", "", `{"hook_event_name":"Stop","session_id":"s1"}`)
	run("status.sh", "2", `{"session_id":"s2"}`)
	run("status.sh", "1", `{"session_id":"s1"}`)
	read := func(rel string) string {
		b, _ := os.ReadFile(filepath.Join(home, ".claude/fleet", rel))
		return string(b)
	}
	var e struct{ Session string }
	if json.Unmarshal([]byte(read("slot-2/events.jsonl")), &e); e.Session != "s2" {
		t.Errorf("slot 2's hook log = %q", read("slot-2/events.jsonl"))
	}
	if json.Unmarshal([]byte(read("events.jsonl")), &e); e.Session != "s1" {
		t.Errorf("slot 1's hook log = %q", read("events.jsonl"))
	}
	if !strings.Contains(read("slot-2/status.json"), "s2") || !strings.Contains(read("status.json"), "s1") {
		t.Errorf("status files: slot 2 %q, slot 1 %q", read("slot-2/status.json"), read("status.json"))
	}
}
