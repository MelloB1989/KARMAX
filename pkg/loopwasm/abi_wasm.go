//go:build wasip1

package loopwasm

import "unsafe"

// The one import. Everything the SDK offers goes through it, so a reviewer
// auditing a loop's reach has exactly one function to look at.
//
//go:wasmimport karmax call
func call(namePtr, nameLen, reqPtr, reqLen, outPtr, outCap uint32) int32

func hostCall(name, payload string, out []byte) int32 {
	np, nl := stringPtr(name)
	rp, rl := stringPtr(payload)
	op, oc := bytesPtr(out)
	return call(np, nl, rp, rl, op, oc)
}

func stringPtr(s string) (uint32, uint32) {
	if len(s) == 0 {
		return 0, 0
	}
	return uint32(uintptr(unsafe.Pointer(unsafe.StringData(s)))), uint32(len(s))
}

func bytesPtr(b []byte) (uint32, uint32) {
	if len(b) == 0 {
		return 0, 0
	}
	return uint32(uintptr(unsafe.Pointer(&b[0]))), uint32(len(b))
}
