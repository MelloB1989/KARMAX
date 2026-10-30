package whatsapp

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"
)

// fakeWacli answers `wacli send` with failure for the first failures calls and
// success after, counting every call. failWith is what wacli prints.
func fakeWacli(t *testing.T, failures int, failWith string) (bin, calls string) {
	t.Helper()
	dir := t.TempDir()
	calls = filepath.Join(dir, "calls")
	bin = filepath.Join(dir, "wacli")
	body := "#!/bin/bash\n" +
		"echo x >> " + calls + "\n" +
		"n=$(wc -l < " + calls + ")\n" +
		"if [ \"$n\" -le " + strconv.Itoa(failures) + " ]; then echo '" + failWith + "'; exit 1; fi\n" +
		"echo sent\n"
	if err := os.WriteFile(bin, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, calls
}

func countCalls(t *testing.T, path string) int {
	t.Helper()
	b, _ := os.ReadFile(path)
	return strings.Count(string(b), "\n")
}

func quickRetries(t *testing.T) {
	t.Helper()
	old := bridgeRetryDelays
	bridgeRetryDelays = []time.Duration{10 * time.Millisecond, 10 * time.Millisecond, 10 * time.Millisecond}
	t.Cleanup(func() { bridgeRetryDelays = old })
}

const target = "15555550101@s.whatsapp.net"

// wacli drops its WhatsApp connection several times a day and is back within
// a second or two. A send in that window was refused with "client is not
// connected" — before anything reached WhatsApp — and the message, an
// operator's alert among them, was simply lost. It is sent again once the
// bridge is back.
func TestASendRefusedWhileTheBridgeReconnectsIsSentAgain(t *testing.T) {
	quickRetries(t)
	bin, calls := fakeWacli(t, 2, "send: WhatsApp client is not connected")
	w := New("wa", bin, "", "", zap.NewNop())

	if err := w.Send(context.Background(), target, "the build is green"); err != nil {
		t.Fatalf("send failed although the bridge came back: %v", err)
	}
	if n := countCalls(t, calls); n != 3 {
		t.Errorf("wacli was called %d times, want 3 (two refusals, then the send)", n)
	}
}

// The daemon itself being down (restarting) is the same: nothing was sent.
func TestASendRefusedBecauseTheDaemonIsDownIsSentAgain(t *testing.T) {
	quickRetries(t)
	bin, calls := fakeWacli(t, 1, `send: Post "http://127.0.0.1:8765/send": dial tcp 127.0.0.1:8765: connect: connection refused`)
	w := New("wa", bin, "", "", zap.NewNop())

	if err := w.Send(context.Background(), target, "hello"); err != nil {
		t.Fatalf("send failed although the daemon came back: %v", err)
	}
	if n := countCalls(t, calls); n != 2 {
		t.Errorf("wacli was called %d times, want 2", n)
	}
}

// A send that may have gone through is never repeated: a timeout waiting for
// wacli's answer says nothing about whether WhatsApp got the message, and a
// person cannot un-receive a duplicate.
func TestASendThatMayHaveGoneThroughIsNotRepeated(t *testing.T) {
	quickRetries(t)
	bin, calls := fakeWacli(t, 5, `send: Post "http://127.0.0.1:8765/send": context deadline exceeded (Client.Timeout exceeded while awaiting headers)`)
	w := New("wa", bin, "", "", zap.NewNop())

	if err := w.Send(context.Background(), target, "hello"); err == nil {
		t.Fatal("expected the timeout to be reported")
	}
	if n := countCalls(t, calls); n != 1 {
		t.Errorf("wacli was called %d times; an ambiguous failure must not be retried", n)
	}
}

// A bridge that stays down is reported, in wacli's words, after a bounded
// number of tries rather than retried for ever.
func TestABridgeThatStaysDownIsReportedAfterBoundedTries(t *testing.T) {
	quickRetries(t)
	bin, calls := fakeWacli(t, 9, "send: WhatsApp client is not connected")
	w := New("wa", bin, "", "", zap.NewNop())

	err := w.Send(context.Background(), target, "hello")
	if err == nil || !strings.Contains(err.Error(), "not connected") {
		t.Fatalf("err = %v, want wacli's own reason", err)
	}
	if n, want := countCalls(t, calls), len(bridgeRetryDelays)+1; n != want {
		t.Errorf("wacli was called %d times, want %d", n, want)
	}
}
