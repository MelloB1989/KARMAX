# WASM extension tier — does it meet its budget?

Answers open question 3 of `KARMAXORCHESTRATIONPLAN.md` before Phase 3 commits
to the design. Budget from §4.3: **under 5 ms overhead per run, under 1 ms per
host call.**

## Answer: yes, with margin, in the worst case

Measured on this machine (Linux amd64, Go 1.26.3, wazero v1.12.0, optimising
compiler backend), with the guest built by the **standard Go toolchain** — the
heaviest guest anyone would realistically produce, 1.8 MB of module because it
carries a full Go runtime and GC.

| | measured | budget | |
|---|---|---|---|
| Per run (instantiate + call + close) | **3.39 ms** | 5 ms | pass |
| Per host call (with a memory read and write) | **1.75 µs** | 1 ms | pass, by ~570× |
| Compile, cold | 650 ms | — | paid once |
| Compile, cached to disk | 26 ms | — | 25× cheaper |

## Where the per-run cost actually goes

```
instantiate  3.445 ms   ← 99% of it
call            42 µs
close            4 µs
```

Instantiation is the whole cost, and instantiation scales with module size. So
the single lever, if this ever gets tight, is a leaner guest: TinyGo, Rust or
hand-written WAT produce modules one to two orders of magnitude smaller than
`GOOS=wasip1` Go. The number above is therefore a ceiling, not an estimate.

For comparison, reusing one instance across runs costs **3.2 µs** — a thousand
times cheaper. That is not a recommendation: a fresh instance per run is what
buys isolation, and 3.4 ms to get it is a good trade for work that fires seconds
to minutes apart. It is worth knowing the number exists if a loop ever needs to
run at a frequency where it does not.

## What this confirms about the design

- **Compile at install, not at run.** 650 ms → 26 ms with `NewCompilationCacheWithDir`.
  Without the cache, every run would blow the budget 130× over on its own.
- **Coarse-grained host calls are the thing that matters, and they are cheap.**
  1.75 µs per crossing including a memory read and a write. A `Recall` returning
  a batch is free; an iterator with a call per item would still be fine at this
  price, though the batch API remains the right shape for other reasons.
- **JSON across the boundary is not worth optimising.** At this call frequency
  it is not close to being the cost.
- **Loops are cold, not hot.** The comparison against a native Go loop shows WASM
  ~3400× slower on pure arithmetic in a tight loop — and that is the honest
  worst case for a synthetic microbenchmark, not for a loop. A real loop's wall
  clock is dominated by LLM calls, HTTP and harness delegation, every one of
  which executes host-side. The guest is glue.

## Caveats worth carrying into Phase 3

1. Measured on amd64 with the optimising backend. arm64 has the same backend;
   exotic arches fall back to the interpreter and would need re-measuring.
2. ~~This spike wires `wasi_snapshot_preview1`, which the plan says the real
   implementation must **not** do.~~ **Resolved in `internal/wasmloop`.** A
   Go-compiled guest cannot initialise without preview1, so the shipped host
   instantiates it too — but the ambient authority comes from wazero's
   `ModuleConfig`, not from WASI's presence. With no FS, no environment and no
   args configured, `path_open` has nothing to open and `environ_get` returns
   empty; sockets are not in preview1 at all. There is a guest in
   `internal/wasmloop/testdata` that tries to escape and a test that fails on
   the word BREACH.
3. The 20 MB memory limit, egress allowlist, and interruption via context
   cancellation were not exercised here; they are correctness features, not
   latency ones, but they should get their own tests when built.

## Reproducing

```sh
cd spikes/wasm
GOOS=wasip1 GOARCH=wasm go build -buildmode=c-shared -o guest.wasm ./guest
go test -v ./...
```

Separate Go module on purpose: a negative answer should have cost the core
nothing, and wazero stays out of the core's `go.mod` until Phase 3 puts it there
deliberately.
