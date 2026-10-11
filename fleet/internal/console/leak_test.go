package console

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

	"github.com/creack/pty"

	"github.com/MelloB1989/karmax/fleet/internal/target"
)

func alive(pid int) bool { return syscall.Kill(pid, 0) == nil }

// startMarked runs argv with the console's marker in its environment.
func startMarked(t *testing.T, marker string, argv ...string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Env = append(os.Environ(), marker)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })
	return cmd
}

// The sweep ends what carries the marker and nothing else.
func TestSweepEndsMarkedProcessesOnly(t *testing.T) {
	marked := startMarked(t, streamEnv+"=old", "sleep", "60")
	unmarked := startMarked(t, "SOMETHING_ELSE=1", "sleep", "61")
	if err := sweepLeftovers(context.Background(), target.Container{}); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { marked.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the marked process survived the sweep")
	}
	if !alive(unmarked.Process.Pid) {
		t.Fatal("the sweep killed a process without the marker")
	}
}

// The tail wrapper ends tail when its stdin closes, though nothing kills it:
// what a killed host-side `docker exec` leaves behind.
func TestEventsScriptEndsWhenStdinCloses(t *testing.T) {
	home := t.TempDir()
	os.MkdirAll(filepath.Join(home, ".claude", "fleet"), 0o755)
	os.WriteFile(filepath.Join(home, ".claude", "fleet", "events.jsonl"), nil, 0o644)
	cmd := exec.Command("sh", "-c", eventsScript)
	cmd.Env = append(os.Environ(), "HOME="+home)
	in, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer cmd.Process.Kill()
	var tailPid int
	waitFor(t, 3*time.Second, "tail to start", func() bool {
		out, _ := exec.Command("pgrep", "-P", strconv.Itoa(cmd.Process.Pid), "tail").Output()
		tailPid, _ = strconv.Atoi(strings.TrimSpace(strings.Split(string(out), "\n")[0]))
		return tailPid > 0
	})
	in.Close()
	waitFor(t, 5*time.Second, "tail to end", func() bool { return !alive(tailPid) || zombie(tailPid) })
	cmd.Wait()
}

func zombie(pid int) bool {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	return err != nil || strings.Contains(string(b), ") Z ")
}

// startSurviving starts the attach like docker does: the process outlives
// what the console holds. It returns a stand-in for the host-side docker
// exec (the thing the console kills) and keeps the PTY's master open.
func startSurviving(t *testing.T) StartFunc {
	return func(argv []string) (*os.File, *exec.Cmd, error) {
		real := exec.Command(argv[0], argv[1:]...)
		real.Env = append(os.Environ(), "TERM=xterm-256color")
		f, err := pty.StartWithSize(real, &pty.Winsize{Rows: 50, Cols: 220})
		if err != nil {
			return nil, nil, err
		}
		keep, _ := syscall.Dup(int(f.Fd()))
		kept := os.NewFile(uintptr(keep), "master")
		t.Cleanup(func() { real.Process.Kill(); kept.Close(); real.Wait() })
		go func() { // someone reads the screen, as the daemon does
			b := make([]byte, 4096)
			for {
				if _, err := kept.Read(b); err != nil {
					return
				}
			}
		}()
		dummy := exec.Command("sleep", "60")
		if err := dummy.Start(); err != nil {
			return nil, nil, err
		}
		return f, dummy, nil
	}
}

// Closing a control session must detach its client even though killing the
// docker exec does not (Docker 29), and must leave a person's own client.
func TestControlEndDetachesItsClientAndOnlyThat(t *testing.T) {
	terms, tm := tmuxEnv(t)
	terms.start = startSurviving(t)

	human := exec.Command("tmux", "attach", "-t", "main")
	human.Env = append(os.Environ(), "TERM=xterm-256color")
	hf, err := pty.StartWithSize(human, &pty.Winsize{Rows: 50, Cols: 220})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { human.Process.Kill(); hf.Close(); human.Wait() })
	go func() {
		b := make([]byte, 4096)
		for {
			if _, err := hf.Read(b); err != nil {
				return
			}
		}
	}()
	waitFor(t, 2*time.Second, "the person's client", func() bool { return rwClients(tm) == 1 })

	_, _, cancel, done := controlIn(t, terms, "alice")
	waitFor(t, 3*time.Second, "alice's client", func() bool { return rwClients(tm) == 2 })
	cancel()
	<-done
	waitFor(t, 3*time.Second, "alice's client to be detached", func() bool { return rwClients(tm) == 1 })
	time.Sleep(300 * time.Millisecond)
	if rwClients(tm) != 1 {
		t.Fatalf("clients = %d: the person's own client must stay", rwClients(tm))
	}
}

// The shared watch client is detached when the last viewer leaves.
func TestWatchEndDetachesItsClient(t *testing.T) {
	terms, tm := tmuxEnv(t)
	terms.start = startSurviving(t)
	_, cancel, done := watchIn(t, terms)
	waitFor(t, 3*time.Second, "a read-only client", func() bool { return strings.Contains(tm("list-clients", "-F", "#{client_readonly}"), "1") })
	cancel()
	<-done
	waitFor(t, 5*time.Second, "the watch client to be detached", func() bool { return strings.TrimSpace(tm("list-clients")) == "" })
}
