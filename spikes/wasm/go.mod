// Throwaway: answers open question 3 of the orchestration plan — whether a
// WASM extension tier can meet its latency budget — before Phase 3 commits to
// it. A separate module so a negative answer costs the core nothing.
module karmax-spike/wasm

go 1.26

require github.com/tetratelabs/wazero v1.12.0

require golang.org/x/sys v0.44.0 // indirect
