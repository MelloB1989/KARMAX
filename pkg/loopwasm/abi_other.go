//go:build !wasip1

package loopwasm

// Off-target, every host call fails the same way. This exists so a loop's logic
// can be unit tested on a normal machine without a wasm toolchain — the tests
// stub what they need and the SDK does not pretend to work.
func hostCall(name, payload string, out []byte) int32 { return -1 }
