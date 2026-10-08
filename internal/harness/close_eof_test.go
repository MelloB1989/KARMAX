package harness

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// writeScript writes an executable bash script and returns its path.
func writeScript(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fake.sh")
	if err := os.WriteFile(path, []byte("#!/bin/bash\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

const resultLine = `echo '{"type":"result","subtype":"success","is_error":false,"result":"ok","usage":{}}'` + "\n"

func openOne(t *testing.T, bin string, pol Policy) *Session {
	t.Helper()
	sup := New(Config{
		Binary: bin, WorkdirRoot: t.TempDir(), MaxLive: 4, Env: os.Environ(),
		Policies: map[string]Policy{"k": pol},
	}, newMemStore(), NewBreaker(0.95, nil), testLog{t}, nil)
	t.Cleanup(sup.Shutdown)
	sess, err := sup.open(context.Background(), "key", "k", sup.policy("k"), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sess.Send(context.Background(), "hi", 5*time.Second, nil); err != nil {
		t.Fatal(err)
	}
	return sess
}

// Close ends its stdin, which is how a stream-json session is told to exit
// politely. A process that ignores SIGINT still goes at once, rather than
// after the SIGKILL fallback.
func TestCloseSendsEOF(t *testing.T) {
	bin := writeScript(t, "trap '' INT\nwhile IFS= read -r line; do "+resultLine+"done\n")
	sess := openOne(t, bin, Policy{TurnTimeout: 5 * time.Second})

	start := time.Now()
	sess.Close()
	if d := time.Since(start); d >= 2*time.Second {
		t.Fatalf("Close took %s: the process never saw EOF and waited out the kill fallback", d)
	}
}

// A session run through a launch prefix (docker exec) is given time to exit on
// EOF before it is signalled: the signal reaches only the prefix's own client,
// and killing that first can strand the real process wherever it runs.
func TestCloseGivesALaunchedSessionTimeToExitOnEOF(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "clean")
	bin := writeScript(t, "while IFS= read -r line; do "+resultLine+"done\nsleep 0.5\ntouch "+marker+"\n")
	sess := openOne(t, bin, Policy{TurnTimeout: 5 * time.Second, Launch: []string{"env"}})

	sess.Close()
	if _, err := os.Stat(marker); err != nil {
		t.Fatal("a launched session was signalled before it could finish exiting on EOF")
	}
}

// A local session is still signalled straight away, as it always was: Close
// is also how a running turn is interrupted, and that must not wait.
func TestCloseStillSignalsALocalSessionAtOnce(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "clean")
	bin := writeScript(t, "while IFS= read -r line; do "+resultLine+"done\nsleep 0.5\ntouch "+marker+"\n")
	sess := openOne(t, bin, Policy{TurnTimeout: 5 * time.Second})

	sess.Close()
	time.Sleep(time.Second)
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("a local session was left to exit on its own; it should have been signalled")
	}
}
