package wasmloop

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MelloB1989/karmax/internal/broker"
	"go.uber.org/zap"
)

func buildProvider(t *testing.T) []byte {
	t.Helper()
	out := filepath.Join(t.TempDir(), "provider.wasm")
	cmd := exec.Command("go", "build", "-buildmode=c-shared", "-o", out, "./testdata/provider")
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

func providerRunner(t *testing.T, provides []ToolSpec) *Runner {
	t.Helper()
	module := buildProvider(t)
	m := Manifest{
		Name: "provider", Version: "1.0.0", Host: []string{FnLog},
		Provides: provides, MemoryMB: 64,
	}
	packedBytes, err := Pack(m, module, nil)
	if err != nil {
		t.Fatal(err)
	}
	a, err := Unpack(packedBytes)
	if err != nil {
		t.Fatal(err)
	}

	brk := broker.New(testStore(t), zap.NewNop())
	brk.SetTrust(broker.LoopSubject(m.Name), broker.Ungated)

	ctx := context.Background()
	r, err := NewRunner(ctx, a, Options{
		Namespace: "nexus", Kit: nullKit{},
		Grants: brk.For(broker.LoopSubject(m.Name)), Log: zap.NewNop(),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close(ctx) })
	return r
}

// The host can call back into a module and get an answer out of it.
func TestTheHostCanInvokeAToolAWorkflowProvides(t *testing.T) {
	r := providerRunner(t, []ToolSpec{{Name: "deal.status", Description: "where a deal stands"}})

	out, err := r.InvokeTool(context.Background(), "deal.status",
		map[string]any{"deal": "CampX"}, 30*time.Second)
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	if !strings.Contains(out, "CampX") || !strings.Contains(out, "stage 3") {
		t.Errorf("the workflow's answer did not come back: %q", out)
	}
}

// A tool the manifest does not declare does not exist, however the module is
// built. The signed manifest is the list the operator approved, and a module
// must not be able to hand the agent a tool that was never on it.
func TestAnUndeclaredProvidedToolIsRefused(t *testing.T) {
	r := providerRunner(t, []ToolSpec{{Name: "deal.status"}})

	// The guest implements this one, but the manifest never declared it.
	if _, err := r.InvokeTool(context.Background(), "always.fails", nil, 30*time.Second); err == nil {
		t.Fatal("a tool absent from the manifest was invoked anyway")
	}
}

// A provided tool runs in a FRESH instance, not the one mid-run.
//
// This is the design decision the whole feature rests on: re-entering a live
// instance means calling a guest export while that instance is suspended
// inside a host call, and whether Go's wasip1 runtime survives that is an open
// question. It also makes the later-turn case — where no instance is running at
// all — the same code path instead of a second one.
func TestAProvidedToolRunsInAFreshInstance(t *testing.T) {
	r := providerRunner(t, []ToolSpec{{Name: "saw.run"}})
	ctx := context.Background()

	if err := r.Run(ctx, 30*time.Second); err != nil {
		t.Fatalf("run: %v", err)
	}
	out, err := r.InvokeTool(ctx, "saw.run", nil, 30*time.Second)
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	if strings.TrimSpace(out) != "fresh" {
		t.Errorf("the tool saw state from a previous run: %q", out)
	}
}

// The deadlock this pins.
//
// A workflow asks the agent something; the agent answers by calling a tool the
// workflow provides. If provided tools shared the mutex a run holds for its
// whole duration, the workflow would block on itself and the turn would hang
// until its timeout — every time, for the single most likely way anyone uses
// this feature.
func TestAProvidedToolCanBeCalledWhileTheLoopIsRunning(t *testing.T) {
	r := providerRunner(t, []ToolSpec{{Name: "deal.status"}})
	ctx := context.Background()

	// Hold the run mutex the way an in-flight run does.
	r.mu.Lock()
	defer r.mu.Unlock()

	done := make(chan error, 1)
	go func() {
		_, err := r.InvokeTool(ctx, "deal.status", map[string]any{"deal": "CampX"}, 20*time.Second)
		done <- err
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("invoke while running: %v", err)
		}
	case <-time.After(25 * time.Second):
		t.Fatal("a provided tool deadlocked against its own loop's run")
	}
}

// A broken tool reports its failure rather than taking the turn down with it.
func TestAFailingProvidedToolReturnsItsErrorToTheModel(t *testing.T) {
	r := providerRunner(t, []ToolSpec{{Name: "always.fails"}})

	out, err := r.InvokeTool(context.Background(), "always.fails", nil, 30*time.Second)
	if err != nil {
		t.Fatalf("the guest's own error should come back as a result, not a host failure: %v", err)
	}
	var res struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("output was not the error envelope: %q", out)
	}
	if !strings.Contains(res.Error, "broken on purpose") {
		t.Errorf("the reason was lost: %q", out)
	}
}

// Two calls at once must not corrupt each other's memory.
func TestConcurrentProvidedToolCallsAreSerialised(t *testing.T) {
	r := providerRunner(t, []ToolSpec{{Name: "deal.status"}})
	ctx := context.Background()

	var wg sync.WaitGroup
	errs := make([]error, 8)
	outs := make([]string, 8)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			outs[i], errs[i] = r.InvokeTool(ctx, "deal.status",
				map[string]any{"deal": "CampX"}, 30*time.Second)
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("call %d failed: %v", i, err)
		}
		if !strings.Contains(outs[i], "stage 3") {
			t.Errorf("call %d came back wrong: %q", i, outs[i])
		}
	}
}
