package console

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/creack/pty"
)

const tmuxConf = "../../../packaging/fleet/tmux.conf"

// syncBuf collects what a viewer is shown, and the frames.
type syncBuf struct {
	mu     sync.Mutex
	b      bytes.Buffer
	frames []TermFrame
}

func (s *syncBuf) out(p []byte)      { s.mu.Lock(); s.b.Write(p); s.mu.Unlock() }
func (s *syncBuf) frame(f TermFrame) { s.mu.Lock(); s.frames = append(s.frames, f); s.mu.Unlock() }
func (s *syncBuf) has(sub string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return strings.Contains(s.b.String(), sub)
}
func (s *syncBuf) hasFrame(typ string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, f := range s.frames {
		if f.Type == typ {
			return true
		}
	}
	return false
}

func waitFor(t *testing.T, d time.Duration, what string, ok func() bool) {
	t.Helper()
	for end := time.Now().Add(d); !ok(); time.Sleep(20 * time.Millisecond) {
		if time.Now().After(end) {
			t.Fatalf("timed out after %v waiting for %s", d, what)
		}
	}
}

// tmuxEnv gives the test its own tmux server (TMUX_TMPDIR) running the
// image's tmux.conf, with session "main" running cat, and a native agent.
func tmuxEnv(t *testing.T) (*Terms, func(args ...string) string) {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("no tmux")
	}
	dir, err := os.MkdirTemp("", "tm")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMUX_TMPDIR", dir)
	t.Setenv("TMUX", "")
	os.Unsetenv("TMUX")
	tm := func(args ...string) string {
		out, _ := exec.Command("tmux", args...).CombinedOutput()
		return string(out)
	}
	t.Cleanup(func() { tm("kill-server"); os.RemoveAll(dir) })
	if out, err := exec.Command("tmux", "-f", tmuxConf, "new-session", "-d", "-s", "main", "-x", "220", "-y", "50", "cat").CombinedOutput(); err != nil {
		t.Fatalf("tmux new-session: %v %s", err, out)
	}
	cfg := &Config{Term: TermSize{Cols: 220, Rows: 50}, Agents: []AgentConf{{Name: "a1", Live: true}}}
	terms := NewTerms(cfg, nil)
	terms.idleClose = 200 * time.Millisecond
	return terms, tm
}

func watchIn(t *testing.T, terms *Terms) (*syncBuf, context.CancelFunc, chan error) {
	t.Helper()
	b := &syncBuf{}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- terms.Watch(ctx, "a1", b.out, b.frame) }()
	t.Cleanup(cancel)
	waitFor(t, 2*time.Second, "hello", func() bool { return b.hasFrame("hello") })
	return b, cancel, done
}

func TestWatchStreamsWhatTheSessionPrints(t *testing.T) {
	terms, tm := tmuxEnv(t)
	b, _, _ := watchIn(t, terms)
	tm("send-keys", "-t", "main", "hello", "Enter")
	waitFor(t, 2*time.Second, "hello on the screen", func() bool { return b.has("hello") })
}

// A slot's terminal is its own pane: watching agent a1's slot 2 shows
// main-2, never slot 1's main.
func TestWatchingASlotShowsItsOwnPane(t *testing.T) {
	terms, tm := tmuxEnv(t)
	if out, err := exec.Command("tmux", "new-session", "-d", "-s", "main-2", "-x", "220", "-y", "50", "cat").CombinedOutput(); err != nil {
		t.Fatalf("tmux new-session: %v %s", err, out)
	}
	terms.cfg.Agents = append(terms.cfg.Agents, AgentConf{Name: "a1:2", Agent: "a1", Slot: 2, Live: true})
	b := &syncBuf{}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = terms.Watch(ctx, "a1:2", b.out, b.frame) }()
	waitFor(t, 2*time.Second, "hello", func() bool { return b.hasFrame("hello") })
	tm("send-keys", "-t", "=main:", "SLOT-ONE", "Enter")
	tm("send-keys", "-t", "=main-2:", "SLOT-TWO", "Enter")
	waitFor(t, 2*time.Second, "slot 2's screen", func() bool { return b.has("SLOT-TWO") })
	if b.has("SLOT-ONE") {
		t.Fatal("slot 2's terminal shows slot 1's pane")
	}
}

func TestWatchSecondSubscriberGetsFullRedraw(t *testing.T) {
	terms, tm := tmuxEnv(t)
	first, _, _ := watchIn(t, terms)
	tm("send-keys", "-t", "main", "MARK", "Enter")
	waitFor(t, 2*time.Second, "MARK for the first viewer", func() bool { return first.has("MARK") })

	second, _, _ := watchIn(t, terms) // nothing new is printed from here on
	waitFor(t, 2*time.Second, "MARK for the second viewer", func() bool { return second.has("MARK") })
}

func TestWatchClientClosesWhenLastViewerLeaves(t *testing.T) {
	terms, tm := tmuxEnv(t)
	_, cancel, done := watchIn(t, terms)
	waitFor(t, 2*time.Second, "a read-only client", func() bool { return strings.Contains(tm("list-clients", "-F", "#{client_readonly}"), "1") })
	cancel()
	<-done
	waitFor(t, 3*time.Second, "the client to close", func() bool { return strings.TrimSpace(tm("list-clients")) == "" })
	terms.mu.Lock()
	n := len(terms.watches)
	terms.mu.Unlock()
	if n != 0 {
		t.Fatalf("%d watches left", n)
	}
}

func controlIn(t *testing.T, terms *Terms, who string) (*syncBuf, chan []byte, context.CancelFunc, chan error) {
	t.Helper()
	b := &syncBuf{}
	in := make(chan []byte, 8)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- terms.Control(ctx, "a1", who, in, b.out, b.frame) }()
	t.Cleanup(cancel)
	return b, in, cancel, done
}

func TestControlTypesIntoTheSession(t *testing.T) {
	terms, tm := tmuxEnv(t)
	_, in, _, _ := controlIn(t, terms, "alice")
	in <- []byte("echo hi\r")
	waitFor(t, 3*time.Second, "the typed line in the pane", func() bool {
		return strings.Contains(tm("capture-pane", "-p", "-t", "main"), "echo hi")
	})
}

func TestOnlyOneController(t *testing.T) {
	terms, _ := tmuxEnv(t)
	controlIn(t, terms, "alice")
	waitFor(t, 2*time.Second, "alice to hold it", func() bool { return terms.Holder("a1") == "alice" })
	err := terms.Control(context.Background(), "a1", "bob", make(chan []byte), func([]byte) {}, func(TermFrame) {})
	var held ErrHeld
	if !errors.As(err, &held) || held.Holder != "alice" {
		t.Fatalf("err = %v, want ErrHeld naming alice", err)
	}
}

func rwClients(tm func(...string) string) int {
	n := 0
	for _, l := range strings.Split(tm("list-clients", "-F", "#{client_readonly}"), "\n") {
		if strings.TrimSpace(l) == "0" {
			n++
		}
	}
	return n
}

func TestControlReleasedWhenSocketDrops(t *testing.T) {
	terms, tm := tmuxEnv(t)
	_, _, cancel, done := controlIn(t, terms, "alice")
	waitFor(t, 2*time.Second, "a read-write client", func() bool { return rwClients(tm) == 1 })
	cancel() // the WebSocket dropped
	<-done

	start := time.Now()
	_, _, cancel2, _ := controlIn(t, terms, "bob")
	waitFor(t, time.Second, "bob to take over", func() bool { return terms.Holder("a1") == "bob" })
	if time.Since(start) > time.Second {
		t.Fatal("slow release")
	}
	cancel2()
	waitFor(t, 2*time.Second, "no read-write client left", func() bool { return rwClients(tm) == 0 })
	if h := terms.Holder("a1"); h != "" {
		waitFor(t, time.Second, "the lock to clear", func() bool { return terms.Holder("a1") == "" })
	}
}

func TestCtrlBracketDetachesNotTmuxPrefix(t *testing.T) {
	terms, tm := tmuxEnv(t)
	b, in, _, done := controlIn(t, terms, "alice")
	waitFor(t, 2*time.Second, "a read-write client", func() bool { return rwClients(tm) == 1 })

	in <- []byte("\x02d") // Ctrl-b d: no prefix any more, so it is just input
	time.Sleep(600 * time.Millisecond)
	if rwClients(tm) != 1 || b.hasFrame("released") {
		t.Fatal("Ctrl-b d detached the client: the tmux prefix is still live")
	}
	in <- []byte("\x1d") // Ctrl-]
	waitFor(t, 3*time.Second, "the released frame", func() bool { return b.hasFrame("released") })
	if err := <-done; err != nil {
		t.Fatalf("Control returned %v", err)
	}
	if rwClients(tm) != 0 || terms.Holder("a1") != "" {
		t.Fatal("still attached or still held after Ctrl-]")
	}
}

func TestWindowStays220x50AfterASmallClient(t *testing.T) {
	_, tm := tmuxEnv(t)
	cmd := exec.Command("tmux", "attach", "-t", "main")
	cmd.Env = append(os.Environ(), "TERM=xterm-256color")
	f, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: 24, Cols: 80})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { cmd.Process.Kill(); f.Close(); cmd.Wait() }()
	go func() {
		b := make([]byte, 4096)
		for {
			if _, err := f.Read(b); err != nil {
				return
			}
		}
	}()
	waitFor(t, 2*time.Second, "the small client", func() bool { return strings.TrimSpace(tm("list-clients")) != "" })
	time.Sleep(300 * time.Millisecond)
	if got := strings.TrimSpace(tm("display", "-p", "-t", "main", "#{window_width}x#{window_height}")); got != "220x50" {
		t.Fatalf("window is %s after an 80x24 client", got)
	}
}
