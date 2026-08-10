package wasmloop

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
)

// Tools a workflow provides to the agent.
//
// A workflow could already ask the agent things. It could not give the agent
// anything to ask back with — so a workflow that knew how to answer "where does
// the CampX deal stand" had to either answer it unprompted or embed the whole
// question in one `ask`. Declaring a tool inverts that: the agent decides when
// it needs the answer, and the workflow supplies it.
//
// The tool runs in a FRESH instance of the workflow's already-compiled module,
// not in the instance that happens to be running. That is the whole design
// decision, and it is worth being explicit about why:
//
//   - The agent turn that calls the tool is often one the workflow itself
//     started, via `ask`. Re-entering the live instance there means calling a
//     guest export while that same instance is suspended inside a host call.
//     Whether Go's wasip1 runtime survives that is an open question, and one we
//     would be betting the tier on.
//   - The other required case — a later turn, like delegation.completed — has
//     no live instance at all. A fresh instance makes both cases the same code
//     path instead of two.
//
// It costs 3.4ms against an already-compiled module (docs/WASM-PERFORMANCE.md),
// which is nothing inside an agent turn. What it does NOT buy is shared state:
// a provided tool cannot see the loop body's variables. Workflows that need to
// share use short-term memory, which is durable and already there.

// ToolExport is the function a module exports to serve its provided tools.
const ToolExport = "tool"

// ProvidedTools are the tools this workflow implements, from its signed
// manifest. Empty for a workflow that provides none, which is most of them.
func (r *Runner) ProvidedTools() []ToolSpec { return r.manifest.Provides }

// InvokeTool runs one of the workflow's provided tools.
//
// It takes toolMu, NOT the mutex Run holds for the whole of a run. A workflow
// whose `ask` triggers its own provided tool would otherwise deadlock against
// itself on the very first call — the run holding the lock, waiting on an agent
// turn, waiting on this.
func (r *Runner) InvokeTool(ctx context.Context, name string, input map[string]any, timeout time.Duration) (string, error) {
	if !r.provides(name) {
		return "", fmt.Errorf("wasmloop: %s does not provide a tool named %q", r.name, name)
	}
	if input == nil {
		input = map[string]any{}
	}
	req, err := json.Marshal(map[string]any{"name": name, "input": input})
	if err != nil {
		return "", err
	}

	r.toolMu.Lock()
	defer r.toolMu.Unlock()

	callCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cfg := wazero.NewModuleConfig().
		WithName("").
		WithStartFunctions("_initialize").
		WithStdout(logWriter{r.log, r.name, "stdout"}).
		WithStderr(logWriter{r.log, r.name, "stderr"}).
		// Both clocks. Nanotime alone leaves time.Now() at the epoch, so every
		// loop that asked what day it was got 1 January 1970 — which reads as a
		// sandbox decision and is simply a missing line. Wall-clock time is not
		// a capability worth withholding: a loop that runs on a schedule has to
		// know when it is.
		WithSysNanotime().
		WithSysWalltime()

	mod, err := r.runtime.InstantiateModule(callCtx, r.compiled, cfg)
	if err != nil {
		return "", fmt.Errorf("wasmloop: %s would not start to serve %s: %w", r.name, name, err)
	}
	defer mod.Close(callCtx)

	fn := mod.ExportedFunction(ToolExport)
	if fn == nil {
		return "", fmt.Errorf("wasmloop: %s declares the tool %s but exports no %q function",
			r.name, name, ToolExport)
	}

	// The request is handed over the same way every host call travels: written
	// into the guest's memory, and the guest given a pointer and a length.
	reqPtr, err := r.writeGuest(callCtx, mod, req)
	if err != nil {
		return "", err
	}

	res, err := fn.Call(callCtx, uint64(reqPtr), uint64(len(req)))
	if err != nil {
		if callCtx.Err() != nil {
			return "", fmt.Errorf("wasmloop: %s took longer than %s to answer %s", r.name, timeout, name)
		}
		return "", fmt.Errorf("wasmloop: %s failed serving %s: %w", r.name, name, err)
	}
	if len(res) == 0 {
		return "", nil
	}

	// The guest returns a packed pointer/length pair: it owns the memory and
	// the host only reads it, so neither side has to free the other's.
	ptr, size := uint32(res[0]>>32), uint32(res[0])
	if size == 0 {
		return "", nil
	}
	if size > maxToolResponse {
		return "", fmt.Errorf("wasmloop: %s returned %d bytes from %s, over the %d-byte limit",
			r.name, size, name, maxToolResponse)
	}
	out, ok := mod.Memory().Read(ptr, size)
	if !ok {
		return "", fmt.Errorf("wasmloop: %s returned a result outside its own memory", r.name)
	}
	return string(out), nil
}

// maxToolResponse bounds what a provided tool may hand back, since the length
// comes from the guest.
const maxToolResponse = 4 << 20

// writeGuest asks the guest to allocate, then copies the request in.
//
// The guest allocates rather than the host picking an address: the module is
// Go, its memory belongs to a garbage collector, and writing into a region the
// GC does not know about is how a heap gets corrupted three calls later.
func (r *Runner) writeGuest(ctx context.Context, mod api.Module, data []byte) (uint32, error) {
	alloc := mod.ExportedFunction(AllocExport)
	if alloc == nil {
		return 0, fmt.Errorf("wasmloop: %s exports no %q function; rebuild it with a current loopwasm",
			r.name, AllocExport)
	}
	res, err := alloc.Call(ctx, uint64(len(data)))
	if err != nil || len(res) == 0 {
		return 0, fmt.Errorf("wasmloop: %s could not allocate %d bytes: %w", r.name, len(data), err)
	}
	ptr := uint32(res[0])
	if !mod.Memory().Write(ptr, data) {
		return 0, fmt.Errorf("wasmloop: %s allocated memory the host cannot write to", r.name)
	}
	return ptr, nil
}

// AllocExport is the guest's allocator, used to hand it a request.
const AllocExport = "karmax_alloc"

func (r *Runner) provides(name string) bool {
	for _, p := range r.manifest.Provides {
		if p.Name == name {
			return true
		}
	}
	return false
}
