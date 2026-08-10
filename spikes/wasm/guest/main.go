//go:build wasip1

// The guest: what a loop looks like once it is orchestration glue rather than
// the work itself. It calls host functions and decides; the LLM calls, HTTP and
// harness delegation all happen host-side.
package main

import "unsafe"

//go:wasmimport karmax recall
func hostRecall(queryPtr, queryLen uint32, outPtr, outCap uint32) uint32

//go:wasmimport karmax notify
func hostNotify(ptr, length uint32)

var buf [4096]byte

func ptrOf(b []byte) (uint32, uint32) {
	if len(b) == 0 {
		return 0, 0
	}
	return uint32(uintptr(unsafe.Pointer(&b[0]))), uint32(len(b))
}

// run is the loop body: recall some context, decide, notify. calls says how
// many host round trips to make, so the per-call cost can be separated from
// the fixed cost of a run.
//
//go:wasmexport run
func run(calls uint32) uint32 {
	query := []byte("what did we agree with the vendor")
	qp, ql := ptrOf(query)
	bp, _ := ptrOf(buf[:])

	var total uint32
	for i := uint32(0); i < calls; i++ {
		n := hostRecall(qp, ql, bp, uint32(len(buf)))
		total += n
	}
	if total > 0 {
		msg := []byte("done")
		mp, ml := ptrOf(msg)
		hostNotify(mp, ml)
	}
	return total
}

func main() {}
