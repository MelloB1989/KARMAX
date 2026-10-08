package harness

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// A kind with no launch prefix runs the binary itself, exactly as before.
func TestLaunchArgvWithoutAPrefixRunsTheBinary(t *testing.T) {
	name, args := launchArgv(nil, "/w", "claude", []string{"--print"})
	if name != "claude" || !slices.Equal(args, []string{"--print"}) {
		t.Fatalf("got %q %q, want claude [--print]", name, args)
	}
}

// A prefix goes before the binary, with {workdir} filled in, so a session can
// run inside a container (docker exec), on another host (ssh) or anywhere else
// an argv prefix reaches.
func TestLaunchArgvPutsThePrefixBeforeTheBinary(t *testing.T) {
	prefix := []string{"docker", "exec", "-i", "-w", "{workdir}", "karmax-brain"}
	name, args := launchArgv(prefix, "/home/u/.karmax/sessions/agent_karmax", "claude", []string{"--print"})
	want := []string{"exec", "-i", "-w", "/home/u/.karmax/sessions/agent_karmax", "karmax-brain", "claude", "--print"}
	if name != "docker" || !slices.Equal(args, want) {
		t.Fatalf("got %q %q, want docker %q", name, args, want)
	}
	if prefix[4] != "{workdir}" {
		t.Error("the policy's own prefix must not be rewritten in place")
	}
}

// --name is what makes a session addressable by a stable name from other
// sessions; a kind that sets none passes no flag at all.
func TestSpawnArgsNameTheSessionOnlyWhenTheKindDoes(t *testing.T) {
	named := spawnArgs(&Session{ID: "x", Name: "karmax"}, false, "")
	if i := slices.Index(named, "--name"); i < 0 || i+1 >= len(named) || named[i+1] != "karmax" {
		t.Errorf("want --name karmax in %q", named)
	}
	if slices.Contains(spawnArgs(&Session{ID: "x"}, false, ""), "--name") {
		t.Error("a kind without a name must not pass --name")
	}
}

// End to end through the supervisor: a kind's launch prefix and name reach the
// process, and the turn still completes through the wrapper.
func TestSupervisorSpawnsThroughTheKindsLaunchPrefix(t *testing.T) {
	dir := t.TempDir()
	argvLog := filepath.Join(dir, "argv")
	wrapper := filepath.Join(dir, "wrap.sh")
	// Records what it was asked to run, drops its own flag, then runs the rest —
	// what docker exec does.
	body := "#!/bin/bash\nprintf '%s\\n' \"$@\" > " + argvLog + "\nshift; exec \"$@\"\n"
	if err := os.WriteFile(wrapper, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	bin := writeFakeClaude(t, time.Millisecond)
	sup := New(Config{
		Binary: bin, WorkdirRoot: t.TempDir(), MaxLive: 4, Env: os.Environ(),
		Policies: map[string]Policy{"agent": {
			TurnTimeout: 5 * time.Second,
			Launch:      []string{wrapper, "--cd={workdir}"},
			Name:        "karmax",
		}},
	}, newMemStore(), NewBreaker(0.95, nil), testLog{t}, nil)
	t.Cleanup(sup.Shutdown)

	wd := filepath.Join(t.TempDir(), "orch")
	turn, err := sup.SendWith(context.Background(), "agent:karmax", "agent", "hi", Options{Workdir: wd})
	if err != nil || turn.Text != "ok" {
		t.Fatalf("turn through the prefix: %q, %v", turn.Text, err)
	}
	raw, err := os.ReadFile(argvLog)
	if err != nil {
		t.Fatalf("the wrapper never ran: %v", err)
	}
	got := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(got) < 2 || got[0] != "--cd="+wd || got[1] != bin {
		t.Fatalf("wrapper argv %q: want --cd=%s then the binary", got, wd)
	}
	if i := slices.Index(got, "--name"); i < 0 || got[i+1] != "karmax" {
		t.Errorf("wrapper argv %q lacks --name karmax", got)
	}
}
