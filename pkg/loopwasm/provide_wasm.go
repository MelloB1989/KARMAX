//go:build wasip1

package loopwasm

import (
	"encoding/json"
	"unsafe"
)

// Serving the tools a workflow provides.
//
// The host calls `tool` on a fresh instance of the module, so nothing set up by
// run() is visible here. That is deliberate — see internal/wasmloop/provided.go
// — and it means a handler must fetch what it needs rather than assume it. The
// short-term memory calls are the way to carry state between the two.

// karmax_alloc gives the host somewhere to put a request.
//
// The host cannot pick an address itself: this is a Go module, its memory
// belongs to a garbage collector, and writing into a region the GC does not
// know about corrupts the heap in a way that surfaces several calls later. The
// buffer is kept alive in `pinned` until the call returns.
//
//go:wasmexport karmax_alloc
func karmaxAlloc(size uint32) uint32 {
	buf := make([]byte, size)
	pinned = append(pinned, buf)
	if size == 0 {
		return 0
	}
	return uint32(uintptr(unsafe.Pointer(&buf[0])))
}

// pinned holds buffers the host wrote into, so the collector does not move or
// reclaim them between the allocation and the call that reads them.
var pinned [][]byte

// result holds the response for the host to read after tool returns. It is a
// package-level variable for the same reason: the host reads guest memory
// AFTER the call has returned, so a local would already be collectable.
var result []byte

// tool serves one provided tool and returns a packed pointer/length pair.
//
//go:wasmexport tool
func tool(reqPtr, reqLen uint32) uint64 {
	// The request has been read; nothing needs pinning past this point.
	defer func() { pinned = pinned[:0] }()

	req := unsafe.String((*byte)(unsafe.Pointer(uintptr(reqPtr))), int(reqLen))
	var in struct {
		Name  string          `json:"name"`
		Input json.RawMessage `json:"input"`
	}
	if err := json.Unmarshal([]byte(req), &in); err != nil {
		return pack(errorResult("malformed tool request: " + err.Error()))
	}

	h, ok := handlers[in.Name]
	if !ok {
		return pack(errorResult("this loop provides no tool named " + in.Name))
	}
	out, err := h(in.Input)
	if err != nil {
		// Returned to the MODEL, not thrown: a tool that failed usually knows
		// something useful about why, and the model can adapt if it is told.
		return pack(errorResult(err.Error()))
	}
	return pack(out)
}

func pack(out []byte) uint64 {
	result = out
	if len(result) == 0 {
		return 0
	}
	return uint64(uintptr(unsafe.Pointer(&result[0])))<<32 | uint64(len(result))
}
