package target

import (
	"context"
	"io"
	"slices"
	"strings"
	"testing"
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
	if !slices.Equal(r.calls[0], []string{"docker", "inspect", "-f", "{{.State.Running}}", "agent-01"}) {
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
