//go:build linux

package harness

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// childScript is a fake CLI that starts two long commands, the way a real
// session runs `pnpm install` for its operator, then answers turns:
//
//   - one stays in the CLI's process group: a background job, which a
//     non-interactive shell starts with SIGINT ignored;
//   - one moves into a session of its own, the way the real CLI starts every
//     shell command it runs (`zsh -c …` under setsid), which a signal to the
//     CLI's group never reaches.
//
// Neither writes to the CLI's stdout, as neither does under the real CLI.
// onInt is what the CLI does on SIGINT; after its last turn it waits for its
// children, so EOF alone does not end it.
func childScript(t *testing.T, onInt string) (bin, groupPID, ownPID string) {
	t.Helper()
	if _, err := exec.LookPath("setsid"); err != nil {
		t.Skip("setsid not installed")
	}
	dir := t.TempDir()
	groupPID, ownPID = filepath.Join(dir, "group.pid"), filepath.Join(dir, "own.pid")
	bin = writeScript(t, onInt+"\n"+
		"sleep 300 >/dev/null 2>&1 &\n"+
		"echo $! > "+groupPID+"\n"+
		"setsid bash -c 'echo $$ > \"$1\"; exec sleep 300' _ "+ownPID+" >/dev/null 2>&1 &\n"+
		"while IFS= read -r line; do "+resultLine+"done\n"+
		"wait\n")
	return bin, groupPID, ownPID
}

// readPID waits for a fake's child to write its pid, and kills that child
// when the test ends whatever happened, so a failing run leaves nothing.
func readPID(t *testing.T, path string) int {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		b, err := os.ReadFile(path)
		if pid, perr := strconv.Atoi(strings.TrimSpace(string(b))); err == nil && perr == nil && pid > 0 {
			t.Cleanup(func() {
				if running(pid) {
					_ = syscall.Kill(pid, syscall.SIGKILL)
				}
			})
			return pid
		}
		if time.Now().After(deadline) {
			t.Fatalf("the fake never wrote %s", path)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// running reports whether pid is a live process: not gone, and not a zombie
// waiting for init to reap it.
func running(pid int) bool {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return false
	}
	i := strings.LastIndexByte(string(b), ')')
	return i >= 0 && !strings.HasPrefix(strings.TrimSpace(string(b[i+1:])), "Z")
}

func gone(pid int, within time.Duration) bool {
	deadline := time.Now().Add(within)
	for running(pid) {
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(20 * time.Millisecond)
	}
	return true
}

// A session runs in a process group of its own, so the group can be signalled
// as one.
func TestASessionLeadsItsOwnProcessGroup(t *testing.T) {
	sess := openOne(t, writeScript(t, "while IFS= read -r line; do "+resultLine+"done\n"), Policy{TurnTimeout: 5 * time.Second})
	pgid, err := syscall.Getpgid(sess.PID())
	if err != nil {
		t.Fatal(err)
	}
	if pgid != sess.PID() {
		t.Fatalf("the session's process group is %d, want its own (%d)", pgid, sess.PID())
	}
}

// Closing a session that exits on SIGINT ends what it started too. Before,
// only the CLI was signalled: it went, and the commands it had started ran on
// with nobody left to stop them.
func TestClosingASessionEndsTheCommandsItStarted(t *testing.T) {
	bin, groupFile, ownFile := childScript(t, "trap 'exit 130' INT")
	sess := openOne(t, bin, Policy{TurnTimeout: 5 * time.Second})
	inGroup, ownSession := readPID(t, groupFile), readPID(t, ownFile)

	sess.Close()
	if !gone(sess.PID(), time.Second) {
		t.Fatal("the session itself is still running")
	}
	if !gone(inGroup, 2*time.Second) {
		t.Error("a command in the session's own process group outlived the session")
	}
	if !gone(ownSession, 2*time.Second) {
		t.Error("a command the session started in a session of its own outlived it")
	}
}

// The real CLI, closed in the middle of a turn that is running a long shell
// command, leaves nothing running: the case of an orchestrator's `pnpm
// install` and a turn that timed out. Spends quota, so it runs only with
// KARMAX_LIVE_HARNESS set.
func TestLiveClosingMidCommandLeavesNothingRunning(t *testing.T) {
	if os.Getenv("KARMAX_LIVE_HARNESS") == "" {
		t.Skip("set KARMAX_LIVE_HARNESS=1 to run against the real CLI (spends quota)")
	}
	if _, err := exec.LookPath("claude"); err != nil {
		t.Skip("claude not installed")
	}
	sup := New(Config{
		Binary: "claude", WorkdirRoot: t.TempDir(), MaxLive: 2, Env: os.Environ(),
		Policies: map[string]Policy{"k": {Model: "haiku", TurnTimeout: 2 * time.Minute}},
	}, newMemStore(), NewBreaker(0.95, nil), testLog{t}, nil)
	t.Cleanup(sup.Shutdown)

	// A length no other process on the machine is sleeping for.
	arg := strconv.Itoa(4000 + int(time.Now().UnixNano()%1000))
	go func() {
		_, _ = sup.Send(context.Background(), "live:pg", "k",
			"Use the Bash tool to run exactly this command and wait for it: sleep "+arg)
	}()
	var pids []int
	deadline := time.Now().Add(90 * time.Second)
	for len(pids) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the session never started the command")
		}
		time.Sleep(200 * time.Millisecond)
		pids = sleeping(arg)
	}
	sup.Close("live:pg")
	for _, pid := range pids {
		if !gone(pid, 2*time.Second) {
			_ = syscall.Kill(pid, syscall.SIGKILL)
			t.Errorf("the command (pid %d) outlived its closed session", pid)
		}
	}
}

// sleeping lists the processes running `sleep arg`.
func sleeping(arg string) []int {
	var out []int
	ents, _ := os.ReadDir("/proc")
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		b, _ := os.ReadFile("/proc/" + e.Name() + "/cmdline")
		if string(b) == "sleep\x00"+arg+"\x00" {
			out = append(out, pid)
		}
	}
	return out
}

// A session that will not exit is killed, and what it started goes with it:
// the force-kill path is where a long command used to be stranded.
func TestForceKillingASessionEndsTheCommandsItStarted(t *testing.T) {
	bin, groupFile, ownFile := childScript(t, "trap '' INT")
	sess := openOne(t, bin, Policy{TurnTimeout: 5 * time.Second})
	inGroup, ownSession := readPID(t, groupFile), readPID(t, ownFile)

	sess.Close()
	if !gone(sess.PID(), time.Second) {
		t.Fatal("the session itself survived its kill")
	}
	if !gone(inGroup, 2*time.Second) {
		t.Error("a command in the session's own process group outlived the kill")
	}
	if !gone(ownSession, 2*time.Second) {
		t.Error("a command the session started in a session of its own outlived the kill")
	}
}
