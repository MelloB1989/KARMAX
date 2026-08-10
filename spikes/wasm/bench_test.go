package wasmspike

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"
)

// The budget from the orchestration plan: under 5ms overhead per run and under
// 1ms per host call. If it cannot be met, the extension design is wrong and
// this is where that surfaces — before anything is ported.
const (
	runBudget      = 5 * time.Millisecond
	hostCallBudget = 1 * time.Millisecond
)

// recallResult stands in for a batch of memory hits. Deliberately coarse: the
// cost is the boundary crossing, so the Kit returns batches, never an iterator
// with a host call per item.
var recallResult = []byte(`[{"id":"1","text":"net-30 agreed with the vendor on 12 June"},{"id":"2","text":"invoice sent 14 June"}]`)

func hostFuncs(ctx context.Context, r wazero.Runtime, calls *int) error {
	_, err := r.NewHostModuleBuilder("karmax").
		NewFunctionBuilder().
		WithFunc(func(ctx context.Context, m api.Module, qPtr, qLen, outPtr, outCap uint32) uint32 {
			*calls++
			if _, ok := m.Memory().Read(qPtr, qLen); !ok {
				return 0
			}
			n := uint32(len(recallResult))
			if n > outCap {
				n = outCap
			}
			if !m.Memory().Write(outPtr, recallResult[:n]) {
				return 0
			}
			return n
		}).Export("recall").
		NewFunctionBuilder().
		WithFunc(func(ctx context.Context, m api.Module, ptr, length uint32) {
			_, _ = m.Memory().Read(ptr, length)
		}).Export("notify").
		Instantiate(ctx)
	return err
}

func newRuntime(t testing.TB, cacheDir string) (wazero.Runtime, wazero.CompiledModule, *int) {
	t.Helper()
	ctx := context.Background()

	cache, err := wazero.NewCompilationCacheWithDir(cacheDir)
	if err != nil {
		t.Fatalf("compilation cache: %v", err)
	}
	r := wazero.NewRuntimeWithConfig(ctx, wazero.NewRuntimeConfig().WithCompilationCache(cache))
	t.Cleanup(func() { r.Close(ctx) })

	if _, err := wasi_snapshot_preview1.Instantiate(ctx, r); err != nil {
		t.Fatalf("wasi: %v", err)
	}
	calls := 0
	if err := hostFuncs(ctx, r, &calls); err != nil {
		t.Fatalf("host module: %v", err)
	}

	wasm, err := os.ReadFile("guest.wasm")
	if err != nil {
		t.Skipf("guest.wasm not built: %v", err)
	}
	compiled, err := r.CompileModule(ctx, wasm)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	return r, compiled, &calls
}

// native is the same loop body in Go, as the thing WASM is measured against.
func native(calls int) int {
	total := 0
	for i := 0; i < calls; i++ {
		total += len(recallResult)
	}
	return total
}

func TestCompilationIsPaidOnceAndCached(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	wasm, err := os.ReadFile("guest.wasm")
	if err != nil {
		t.Skipf("guest.wasm not built: %v", err)
	}

	compile := func() time.Duration {
		cache, _ := wazero.NewCompilationCacheWithDir(dir)
		r := wazero.NewRuntimeWithConfig(ctx, wazero.NewRuntimeConfig().WithCompilationCache(cache))
		defer r.Close(ctx)
		start := time.Now()
		if _, err := r.CompileModule(ctx, wasm); err != nil {
			t.Fatalf("compile: %v", err)
		}
		return time.Since(start)
	}

	cold := compile()
	warm := compile()
	t.Logf("compile cold=%v  warm(cached)=%v  module=%d KB", cold, warm, len(wasm)/1024)
	if warm > cold {
		t.Errorf("the compilation cache did not help: cold %v, warm %v", cold, warm)
	}
}

func TestPerRunOverheadIsWithinBudget(t *testing.T) {
	r, compiled, _ := newRuntime(t, t.TempDir())
	ctx := context.Background()

	const runs = 30
	var total time.Duration
	for i := 0; i < runs; i++ {
		start := time.Now()
		mod, err := r.InstantiateModule(ctx, compiled, wazero.NewModuleConfig().WithName("").WithStartFunctions("_initialize"))
		if err != nil {
			t.Fatalf("instantiate: %v", err)
		}
		if _, err := mod.ExportedFunction("run").Call(ctx, 0); err != nil {
			t.Fatalf("call: %v", err)
		}
		mod.Close(ctx)
		total += time.Since(start)
	}
	per := total / runs
	t.Logf("per-run overhead (instantiate + call + close): %v   budget %v", per, runBudget)
	if per > runBudget {
		t.Errorf("BUDGET MISSED: %v per run exceeds %v", per, runBudget)
	}
}

func TestHostCallCostIsWithinBudget(t *testing.T) {
	r, compiled, calls := newRuntime(t, t.TempDir())
	ctx := context.Background()

	mod, err := r.InstantiateModule(ctx, compiled, wazero.NewModuleConfig().WithName("").WithStartFunctions("_initialize"))
	if err != nil {
		t.Fatalf("instantiate: %v", err)
	}
	defer mod.Close(ctx)
	fn := mod.ExportedFunction("run")

	// Warm, then measure a large batch so the fixed call cost is amortised out.
	if _, err := fn.Call(ctx, 100); err != nil {
		t.Fatalf("warmup: %v", err)
	}
	const n = 10000
	before := *calls
	start := time.Now()
	if _, err := fn.Call(ctx, n); err != nil {
		t.Fatalf("call: %v", err)
	}
	elapsed := time.Since(start)
	if got := *calls - before; got != n {
		t.Fatalf("host was called %d times, expected %d", got, n)
	}
	per := elapsed / n
	t.Logf("per host call (with a memory read and write each): %v   budget %v", per, hostCallBudget)
	if per > hostCallBudget {
		t.Errorf("BUDGET MISSED: %v per host call exceeds %v", per, hostCallBudget)
	}
}

func TestWasmVersusNative(t *testing.T) {
	r, compiled, _ := newRuntime(t, t.TempDir())
	ctx := context.Background()
	mod, err := r.InstantiateModule(ctx, compiled, wazero.NewModuleConfig().WithName("").WithStartFunctions("_initialize"))
	if err != nil {
		t.Fatalf("instantiate: %v", err)
	}
	defer mod.Close(ctx)
	fn := mod.ExportedFunction("run")

	const calls = 1000
	if _, err := fn.Call(ctx, calls); err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	for i := 0; i < 100; i++ {
		native(calls)
	}
	nativeTime := time.Since(start) / 100

	start = time.Now()
	for i := 0; i < 100; i++ {
		if _, err := fn.Call(ctx, calls); err != nil {
			t.Fatal(err)
		}
	}
	wasmTime := time.Since(start) / 100

	t.Logf("%d host calls — native %v, wasm %v (%.1fx)", calls, nativeTime, wasmTime,
		float64(wasmTime)/float64(max(nativeTime, 1)))
	t.Log("a real loop's wall clock is dominated by LLM, HTTP and harness calls, all host-side")
}

func max(a, b time.Duration) time.Duration {
	if a > b {
		return a
	}
	return b
}

// TestWhereTheRunOverheadGoes splits the per-run cost, because "3ms" is not
// actionable and "instantiate is 95% of it" is.
func TestWhereTheRunOverheadGoes(t *testing.T) {
	r, compiled, _ := newRuntime(t, t.TempDir())
	ctx := context.Background()
	cfg := wazero.NewModuleConfig().WithName("").WithStartFunctions("_initialize")

	const runs = 30
	var inst, call, closed time.Duration
	for i := 0; i < runs; i++ {
		s := time.Now()
		mod, err := r.InstantiateModule(ctx, compiled, cfg)
		if err != nil {
			t.Fatal(err)
		}
		inst += time.Since(s)

		s = time.Now()
		if _, err := mod.ExportedFunction("run").Call(ctx, 1); err != nil {
			t.Fatal(err)
		}
		call += time.Since(s)

		s = time.Now()
		mod.Close(ctx)
		closed += time.Since(s)
	}
	t.Logf("instantiate %v | call %v | close %v  (per run, guest built with the standard Go toolchain)",
		inst/runs, call/runs, closed/runs)

	// Reusing one instance across runs, for comparison — cheaper, but it gives
	// up the isolation that a fresh instance per run buys.
	mod, err := r.InstantiateModule(ctx, compiled, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer mod.Close(ctx)
	fn := mod.ExportedFunction("run")
	if _, err := fn.Call(ctx, 1); err != nil {
		t.Fatal(err)
	}
	s := time.Now()
	for i := 0; i < runs; i++ {
		if _, err := fn.Call(ctx, 1); err != nil {
			t.Fatal(err)
		}
	}
	t.Logf("reusing one instance: %v per run", time.Since(s)/runs)
}
