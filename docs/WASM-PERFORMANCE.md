# The WASM extension tier: what it costs

Measured before the tier was committed to, kept because the numbers still
justify decisions in the code. The spike that produced them
(`spikes/wasm/`) has been deleted — it answered its question.

Measured on Linux amd64, Go 1.26.3, wazero v1.12.0 (optimising backend), with
the guest built by the **standard Go toolchain** — the heaviest guest anyone
would realistically produce, ~1.8MB of module because it carries a full Go
runtime and GC. Budget was 5ms per run and 1ms per host call.

| | measured | budget | |
|---|---|---|---|
| Per run (instantiate + call + close) | **3.39 ms** | 5 ms | pass |
| Per host call (with a memory read and write) | **1.75 µs** | 1 ms | pass, by ~570× |
| Compile, cold | 650 ms | — | paid once, at install |
| Compile, cached to disk | 26 ms | — | 25× cheaper |
| Reusing one instance across runs | 3.2 µs | — | not taken; see below |

Instantiation is 99% of the per-run cost and scales with module size, so the
3.39ms is a ceiling rather than an estimate — TinyGo or Rust guests are one to
two orders of magnitude smaller.

## What the code does because of these numbers

**Compile at install, not at run.** 650ms → 26ms with a disk compilation cache
(`wazero.NewCompilationCacheWithDir`, wired in `wasmloop.NewRunner`). Without
it every run would blow the budget 130× over on its own.

**A fresh instance per run, and per provided-tool call.** Reusing one instance
is a thousand times cheaper and is still not what happens: a fresh instance is
what stops a loop carrying state or corruption from one run into the next, and
3.39ms is a good price for that when the work fires seconds to minutes apart.
It is also why a workflow's provided tools run in their own instance rather than
re-entering a live one — see `internal/wasmloop/provided.go`.

**Coarse host calls, but not because crossings are expensive.** 1.75µs per
crossing including a memory read and write means a `recall` returning a batch is
free and a per-item iterator would still be fine. The ABI is coarse because a
chatty ABI is a large one, and a large ABI is one nobody reads before approving
an install.

**JSON across the boundary was not worth optimising.** At this call frequency it
is nowhere near the cost.

## Reproducing

The spike is gone, but the shape was: build a guest with
`GOOS=wasip1 GOARCH=wasm go build -buildmode=c-shared`, then benchmark
`CompileModule` once against `InstantiateModule` + `ExportedFunction().Call()` +
`Close()` in a loop, with and without a `CompilationCache`.
