package target

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"
)

// fakeRunner records every command and answers from a script keyed by the
// command's last words.
type fakeRunner struct {
	calls  [][]string
	stdins []string
	answer func(argv []string) (string, error)
}

func (f *fakeRunner) Run(_ context.Context, argv []string, stdin io.Reader) ([]byte, error) {
	f.calls = append(f.calls, argv)
	in := ""
	if stdin != nil {
		b, _ := io.ReadAll(stdin)
		in = string(b)
	}
	f.stdins = append(f.stdins, in)
	if f.answer == nil {
		return nil, nil
	}
	out, err := f.answer(argv)
	return []byte(out), err
}

func TestExecPutsTheHostsPrefixFirst(t *testing.T) {
	r := &fakeRunner{}
	c := Container{Exec: []string{"docker", "--context", "pc2"}, Name: "agent-05", Runner: r}
	if _, err := c.Run(context.Background(), "tmux", "has-session", "-t", "main"); err != nil {
		t.Fatal(err)
	}
	want := []string{"docker", "--context", "pc2", "exec", "agent-05", "tmux", "has-session", "-t", "main"}
	if !slices.Equal(r.calls[0], want) {
		t.Fatalf("argv = %q\nwant   %q", r.calls[0], want)
	}
}

func TestExecWithStdinAsksForIt(t *testing.T) {
	r := &fakeRunner{}
	c := Container{Exec: []string{"docker"}, Name: "agent-01", Runner: r}
	if _, err := c.RunIn(context.Background(), strings.NewReader("hello"), "sh", "-c", "cat > /x"); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(r.calls[0][:4], []string{"docker", "exec", "-i", "agent-01"}) || r.stdins[0] != "hello" {
		t.Fatalf("argv = %q stdin = %q", r.calls[0], r.stdins[0])
	}
}

func TestRunningReadsTheContainersState(t *testing.T) {
	r := &fakeRunner{answer: func(argv []string) (string, error) { return "true\n", nil }}
	c := Container{Exec: []string{"docker"}, Name: "agent-01", Runner: r}
	up, err := c.Running(context.Background())
	if err != nil || !up {
		t.Fatalf("up=%v err=%v", up, err)
	}
	if want := []string{"docker", "inspect", "-f", "{{.State.Running}}\t{{.State.Status}}\t{{.State.Error}}", "agent-01"}; !slices.Equal(r.calls[0], want) {
		t.Fatalf("argv = %q", r.calls[0])
	}
}

// A container Docker failed to start (its bind mount's source was missing
// after a power cut) carries Docker's error; one stopped on purpose does not.
func TestStateSaysWhyDockerLeftAContainerStopped(t *testing.T) {
	for _, tc := range []struct{ out, failed string }{
		{"false\texited\terror mounting \"/home/op/code\": no such device\n", `error mounting "/home/op/code": no such device`},
		{"false\texited\t\n", ""},
		{"false\trestarting\tsome error\n", ""}, // Docker is still retrying it
		{"true\trunning\t\n", ""},
	} {
		r := &fakeRunner{answer: func([]string) (string, error) { return tc.out, nil }}
		s, err := Container{Exec: []string{"docker"}, Name: "agent-01", Runner: r}.State(context.Background())
		if err != nil || s.Failed != tc.failed || s.Running != strings.HasPrefix(tc.out, "true") {
			t.Errorf("%q: state = %+v, %v", tc.out, s, err)
		}
	}
}

func TestStartStartsTheContainerOnItsHost(t *testing.T) {
	r := &fakeRunner{}
	if err := (Container{Exec: []string{"docker", "--context", "pc2"}, Name: "agent-05", Runner: r}).Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if want := []string{"docker", "--context", "pc2", "start", "agent-05"}; !slices.Equal(r.calls[0], want) {
		t.Fatalf("argv = %q", r.calls[0])
	}
}

func TestAttachArgv(t *testing.T) {
	c := Container{Exec: []string{"docker", "--context", "pc2"}, Name: "agent-05"}
	got := c.InteractiveArgv("tmux", "attach", "-t", "main")
	want := []string{"docker", "--context", "pc2", "exec", "-it", "agent-05", "tmux", "attach", "-t", "main"}
	if !slices.Equal(got, want) {
		t.Fatalf("argv = %q", got)
	}
}

// A Container with no exec prefix and no name is this machine itself: the
// orchestrator's session when it runs on fleetd's host, outside any container.
func TestANativeTargetRunsOnThisMachine(t *testing.T) {
	r := &fakeRunner{}
	c := Container{Runner: r}
	if _, err := c.RunInEnv(context.Background(), []string{"FLEET_ROLE=relay"}, strings.NewReader("hi"), "claude", "-p"); err != nil {
		t.Fatal(err)
	}
	if want := []string{"env", "FLEET_ROLE=relay", "claude", "-p"}; !slices.Equal(r.calls[0], want) {
		t.Fatalf("argv = %q\nwant   %q", r.calls[0], want)
	}
	if r.stdins[0] != "hi" {
		t.Fatalf("stdin = %q", r.stdins[0])
	}
	if _, err := c.Run(context.Background(), "tmux", "ls"); err != nil {
		t.Fatal(err)
	}
	if want := []string{"tmux", "ls"}; !slices.Equal(r.calls[1], want) {
		t.Fatalf("argv = %q\nwant   %q", r.calls[1], want)
	}
	if up, err := c.Running(context.Background()); err != nil || !up {
		t.Fatalf("a native target is always running: %v %v", up, err)
	}
}

func TestRunReturnsWhenAGrandchildHoldsThePipe(t *testing.T) {
	// `docker --context pc2` is docker running ssh: killing docker leaves ssh
	// holding stdout. Here sh forks a sleep that inherits the pipe.
	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	_, err := Exec{}.Run(ctx, []string{"sh", "-c", "sleep 60 & sleep 60"}, nil)
	if took := time.Since(start); took > WaitDelay+2*time.Second {
		t.Fatalf("Run took %v to give up", took)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want a context error", err)
	}
	if took := time.Since(start); took > 2*time.Second {
		t.Logf("took %v: the group kill should make this quick", took)
	}
}

func TestRunGivesUpOnAGrandchildInAnotherSession(t *testing.T) {
	if testing.Short() {
		t.Skip("waits out WaitDelay")
	}
	// setsid puts the sleep beyond the group kill; only WaitDelay frees Wait.
	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	_, err := Exec{}.Run(ctx, []string{"sh", "-c", "setsid sleep 7 & sleep 60"}, nil)
	if took := time.Since(start); took > WaitDelay+2*time.Second {
		t.Fatalf("Run took %v to give up", took)
	}
	if err == nil {
		t.Fatal("want an error")
	}
}

func TestKillGroupKillsChildren(t *testing.T) {
	cmd := Harden(exec.Command("sh", "-c", "sleep 60 & echo $!; wait"))
	out, _ := cmd.StdoutPipe()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	var pid int
	fmt.Fscan(out, &pid)
	if err := KillGroup(cmd); err != nil {
		t.Fatal(err)
	}
	cmd.Wait()
	for i := 0; i < 50 && syscall.Kill(pid, 0) == nil; i++ {
		time.Sleep(20 * time.Millisecond)
	}
	if syscall.Kill(pid, 0) == nil {
		t.Fatalf("grandchild %d survived KillGroup", pid)
	}
}

// A container that is not there (fleetctl down removed it) is stopped, with
// nothing to start. Any other failure is the host not answering, which says
// nothing about the container: an error.
func TestStateTellsAMissingContainerFromAnUnreachableHost(t *testing.T) {
	for _, tc := range []struct {
		err     string
		wantErr bool
	}{
		{"docker --context pc2 inspect: exit status 1: Error: No such object: agent-01", false},
		{"docker inspect: exit status 1: Error response from daemon: No such container: agent-01", false},
		{"docker --context pc2 inspect: exit status 1: error during connect: ssh: connect to host 192.0.2.26 port 22: Network is unreachable", true},
		{"docker inspect: exit status 1: Cannot connect to the Docker daemon at unix:///var/run/docker.sock. Is the docker daemon running?", true},
	} {
		r := &fakeRunner{answer: func([]string) (string, error) { return "", errors.New(tc.err) }}
		s, err := Container{Exec: []string{"docker"}, Name: "agent-01", Runner: r}.State(context.Background())
		if (err != nil) != tc.wantErr || s.Running || s.Failed != "" {
			t.Errorf("%q: state %+v, err %v; want err=%v", tc.err, s, err, tc.wantErr)
		}
	}
}
