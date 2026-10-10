//go:build !windows

package proctrack

import (
	"bufio"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// leader starts `sh -c "sleep 300 & echo $!; wait"` as a group leader and
// returns the command and the pid of the grandchild sleep.
func leader(t *testing.T) (*exec.Cmd, int) {
	t.Helper()
	cmd := exec.Command("sh", "-c", "sleep 300 & echo $!; wait")
	SetGroup(cmd)
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	line, _ := bufio.NewReader(out).ReadString('\n')
	child, err := strconv.Atoi(strings.TrimSpace(line))
	if err != nil {
		t.Fatalf("grandchild pid %q: %v", line, err)
	}
	t.Cleanup(func() { KillGroup(cmd.Process.Pid); _ = cmd.Wait() })
	return cmd, child
}

func gone(pid int) bool {
	for i := 0; i < 50; i++ {
		if _, alive := processStartAlive(pid); !alive {
			return true
		}
		time.Sleep(50 * time.Millisecond)
	}
	return false
}

func TestRegisterWritesAndUnregisterClears(t *testing.T) {
	dir := t.TempDir()
	Init(dir, nil)
	cmd, _ := leader(t)
	Register(cmd)
	b, _ := os.ReadFile(filepath.Join(dir, FileName))
	if !strings.Contains(string(b), strconv.Itoa(cmd.Process.Pid)) {
		t.Fatalf("pidfile %q lacks the pid", b)
	}
	Unregister(cmd)
	b, _ = os.ReadFile(filepath.Join(dir, FileName))
	if strings.Contains(string(b), strconv.Itoa(cmd.Process.Pid)) {
		t.Fatalf("pidfile %q still has the pid", b)
	}
}

func TestInitKillsTheWholeGroupAPreviousEngineLeft(t *testing.T) {
	dir := t.TempDir()
	Init(dir, nil)
	cmd, child := leader(t)
	Register(cmd)

	// A new engine process starting up: same pidfile, fresh state.
	mu.Lock()
	entries = map[int]Entry{}
	mu.Unlock()
	killed := Init(dir, nil)
	if len(killed) != 1 || killed[0].PID != cmd.Process.Pid {
		t.Fatalf("killed = %+v", killed)
	}
	_ = cmd.Wait()
	if !gone(child) {
		t.Fatal("the grandchild survived: only the direct child was killed")
	}
}

func TestInitKillsSurvivorsOfADeadLeader(t *testing.T) {
	dir := t.TempDir()
	Init(dir, nil)
	cmd, child := leader(t)
	Register(cmd)
	_ = syscall.Kill(cmd.Process.Pid, syscall.SIGKILL) // leader only
	_ = cmd.Wait()
	if _, alive := processStartAlive(child); !alive {
		t.Skip("grandchild died with its leader")
	}
	mu.Lock()
	entries = map[int]Entry{}
	mu.Unlock()
	Init(dir, nil)
	if !gone(child) {
		t.Fatal("a survivor of the recorded group was not killed")
	}
}

func TestInitNeverKillsAReusedPid(t *testing.T) {
	dir := t.TempDir()
	Init(dir, nil)
	bystander := exec.Command("sleep", "300")
	SetGroup(bystander)
	if err := bystander.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { KillGroup(bystander.Process.Pid); _ = bystander.Wait() }()
	pid := bystander.Process.Pid

	// A stale record that names the bystander's pid with a different start time.
	stale := `[{"pid":` + strconv.Itoa(pid) + `,"pgid":` + strconv.Itoa(pid) + `,"start":"Mon Jan  1 00:00:00 2001","cmd":"claude"}]`
	if err := os.WriteFile(filepath.Join(dir, FileName), []byte(stale), 0o600); err != nil {
		t.Fatal(err)
	}
	if killed := Init(dir, nil); len(killed) != 0 {
		t.Fatalf("killed a process that merely reused the pid: %+v", killed)
	}
	if _, alive := processStartAlive(pid); !alive {
		t.Fatal("the bystander is dead")
	}
	// A record without a start time is not a match either.
	_ = os.WriteFile(filepath.Join(dir, FileName), []byte(`[{"pid":`+strconv.Itoa(pid)+`,"pgid":`+strconv.Itoa(pid)+`}]`), 0o600)
	if killed := Init(dir, nil); len(killed) != 0 {
		t.Fatalf("killed on an unknown start time: %+v", killed)
	}
}

func TestInitIgnoresAMissingOrCorruptFile(t *testing.T) {
	dir := t.TempDir()
	if k := Init(dir, nil); len(k) != 0 {
		t.Fatal("nothing to kill")
	}
	_ = os.WriteFile(filepath.Join(dir, FileName), []byte("not json"), 0o600)
	if k := Init(dir, nil); len(k) != 0 {
		t.Fatal("corrupt file must be ignored")
	}
}
