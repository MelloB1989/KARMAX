package wasmloop

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/MelloB1989/karmax/internal/broker"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// End to end with a real compiled module.
//
// Everything else in this package tests what reaches the compiler. This tests
// what the compiled thing can actually do — which is the claim that matters,
// and the one that cannot be checked by reading the host code.

// buildGuest compiles the escape-attempting guest.
func buildGuest(t *testing.T) []byte {
	t.Helper()
	out := filepath.Join(t.TempDir(), "guest.wasm")
	cmd := exec.Command("go", "build", "-buildmode=c-shared", "-o", out, "./testdata/guest")
	cmd.Env = append(os.Environ(), "GOOS=wasip1", "GOARCH=wasm")
	if combined, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("cannot build the wasip1 guest here: %v\n%s", err, combined)
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// nullKit answers host calls without doing anything, since this test is about
// the boundary rather than what is behind it.
type nullKit struct{}

func (nullKit) Recall(string, int) ([]string, error)        { return []string{"a memory"}, nil }
func (nullKit) Remember(string) error                       { return nil }
func (nullKit) Notify(string, string) error                 { return nil }
func (nullKit) Ask(context.Context, string) (string, error) { return "an answer", nil }
func (nullKit) HTTP(context.Context, string, string, map[string]string, string) (string, int, error) {
	return "{}", 200, nil
}

func (nullKit) Config(string) string     { return "" }
func (nullKit) HostTool(n string) string { return "/usr/bin/" + n }

func (nullKit) Harness(context.Context, string) (string, error) { return "harness output", nil }
func (nullKit) Gateway(context.Context, string, ...string) (string, error) {
	return "gateway output", nil
}
func (nullKit) Summarize(context.Context, string) (string, error) { return "a summary", nil }

func (nullKit) Propose(string, string, string) error                       { return nil }
func (nullKit) Remind(string, string, string) error                        { return nil }
func (nullKit) SendWhatsApp(context.Context, string, string, string) error { return nil }
func (nullKit) ReadWhatsApp(context.Context, string, int) (string, error) {
	return "recent messages", nil
}

func (nullKit) ShortSet(string, string, string, int) error    { return nil }
func (nullKit) ShortGet(string, string) (string, bool, error) { return "", false, nil }
func (nullKit) ShortAll(string) ([]ShortMemory, error)        { return nil, nil }
func (nullKit) ChatSummary(string) (*ChatSummary, error)      { return nil, nil }
func (nullKit) SaveChatSummary(ChatSummary) error             { return nil }
func (nullKit) RunLoop(string) error                          { return nil }
func (nullKit) ShortForget(string, string) error              { return nil }
func (nullKit) OperatorChats() []string                       { return []string{"911234567890"} }
func (nullKit) MonitoredChats(context.Context) ([]string, error) {
	return []string{"someone@s.whatsapp.net"}, nil
}
func (nullKit) WhatsAppChats(context.Context, int) (string, error) { return "[]", nil }
func (nullKit) WhatsAppMessages(context.Context, string, int, bool) (string, error) {
	return "[]", nil
}
func (nullKit) GoogleChatSpaces(context.Context) (string, error) { return "[]", nil }

func TestAGuestCannotEscapeTheSandbox(t *testing.T) {
	module := buildGuest(t)

	// The manifest declares ONLY log. Recall is deliberately absent.
	m := Manifest{
		Name: "escape-attempt", Version: "1.0.0",
		Publisher: "unused-here",
		Host:      []string{FnLog},
		MemoryMB:  32,
	}
	packedBytes, err := Pack(m, module, nil)
	if err != nil {
		t.Fatal(err)
	}
	a, err := Unpack(packedBytes)
	if err != nil {
		t.Fatal(err)
	}

	core, logs := observer.New(zapcore.InfoLevel)
	log := zap.New(core)

	brk := broker.New(nil, log)
	brk.SetTrust(broker.LoopSubject(m.Name), broker.Ungated)

	ctx := context.Background()
	r, err := NewRunner(ctx, a, Options{
		Namespace: "test", Kit: nullKit{},
		Grants: brk.For(broker.LoopSubject(m.Name)), Log: log,
	})
	if err != nil {
		t.Fatalf("the guest would not compile or start: %v", err)
	}
	defer r.Close(ctx)

	if err := r.Run(ctx, 30*time.Second); err != nil {
		t.Fatalf("the guest failed to run: %v", err)
	}

	var said []string
	for _, entry := range logs.All() {
		for _, f := range entry.Context {
			if f.Key == "message" {
				said = append(said, f.String)
			}
		}
	}
	transcript := strings.Join(said, "\n")
	t.Logf("guest said:\n%s", transcript)

	if strings.Contains(transcript, "BREACH") {
		t.Fatalf("the guest escaped the sandbox:\n%s", transcript)
	}
	// Every check must have actually run — a guest that crashed early would
	// also produce no BREACH, and that would be a false pass.
	for _, want := range []string{
		"no filesystem",
		"cannot list directories",
		"cannot create files",
		"no environment",
		"undeclared host function refused",
		"declared host function works",
	} {
		if !strings.Contains(transcript, want) {
			t.Errorf("the guest never got as far as %q:\n%s", want, transcript)
		}
	}
}

func TestAGuestThatRunsTooLongIsStopped(t *testing.T) {
	module := buildGuest(t)
	m := Manifest{Name: "slow", Version: "1.0.0", Host: []string{FnLog}, MemoryMB: 32}
	packedBytes, _ := Pack(m, module, nil)
	a, _ := Unpack(packedBytes)

	log := zap.NewNop()
	brk := broker.New(nil, log)
	brk.SetTrust(broker.LoopSubject(m.Name), broker.Ungated)

	ctx := context.Background()
	r, err := NewRunner(ctx, a, Options{Namespace: "test", Kit: nullKit{},
		Grants: brk.For(broker.LoopSubject(m.Name)), Log: log})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close(ctx)

	// A nanosecond is not enough for anything, so this exercises the stop path
	// rather than waiting for a genuinely slow guest.
	err = r.Run(ctx, time.Nanosecond)
	if err == nil {
		t.Fatal("a guest past its deadline was allowed to finish")
	}
	if !strings.Contains(err.Error(), "limit") && !strings.Contains(err.Error(), "context") {
		t.Errorf("the timeout was not reported as one: %v", err)
	}
}

func TestAModuleModifiedOnDiskNeverReachesTheCompiler(t *testing.T) {
	module := buildGuest(t)
	m := Manifest{Name: "tampered", Version: "1.0.0", Host: []string{FnLog}}
	packedBytes, _ := Pack(m, module, nil)
	a, _ := Unpack(packedBytes)

	// Someone edits the module after it was verified at install.
	a.Module = append(append([]byte{}, a.Module...), 0x00)

	log := zap.NewNop()
	brk := broker.New(nil, log)
	_, err := NewRunner(context.Background(), a, Options{Namespace: "test", Kit: nullKit{},
		Grants: brk.For("loop:tampered"), Log: log})
	if err == nil {
		t.Fatal("a modified module was compiled")
	}
	if !strings.Contains(err.Error(), "modified on disk") {
		t.Errorf("the refusal does not explain itself: %v", err)
	}
}
