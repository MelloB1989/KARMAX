package wasmloop

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/MelloB1989/karmax/internal/broker"
	"github.com/MelloB1989/karmax/internal/safety"
	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"
	"go.uber.org/zap"
)

// The host surface.
//
// ONE import: karmax.call(name, request) -> response. One function to audit,
// and every call is checked by name against the manifest and by capability
// against the Broker before anything happens.
//
// Coarse-grained on purpose. The spike measured the boundary at 1.75µs, so the
// cost is not the crossing — it is that a chatty ABI is a large ABI, and a
// large ABI is one nobody reads before approving.

// Host function names. A closed set: an unknown name is refused, not ignored,
// so a module built against a newer KARMAX fails visibly here.
const (
	FnLog      = "log"
	FnRecall   = "recall"
	FnRemember = "remember"
	FnNotify   = "notify"
	FnHTTP     = "http"
	FnTrigger  = "trigger"
	FnAsk      = "ask"
)

var hostDescriptions = map[string]string{
	FnLog:      "write to KARMAX's log",
	FnRecall:   "read your long-term memory",
	FnRemember: "write to your long-term memory",
	FnNotify:   "send you notifications",
	FnHTTP:     "use the network — but only the hosts listed above",
	FnTrigger:  "see what triggered it",
	FnAsk:      "ask your agent questions (which can use its tools)",
}

// capabilityFor maps a host function to the Broker capability it needs, so the
// two cannot drift apart.
var capabilityFor = map[string]func(*Runner) (class, value string){
	FnRecall:   func(r *Runner) (string, string) { return "memory", r.namespace },
	FnRemember: func(r *Runner) (string, string) { return "memory", r.namespace + ":write" },
	FnNotify:   func(r *Runner) (string, string) { return "tool", "app.push" },
	FnAsk:      func(r *Runner) (string, string) { return "tool", "agent.ask" },
}

// Error codes returned to the guest. Negative so a length can be positive.
const (
	errGeneric      = -1
	errNotDeclared  = -2
	errNotPermitted = -3
	errTooSmall     = -4
	errBadRequest   = -5
)

// Kit is what the host does on the guest's behalf. Injected so the runner has
// no opinion about where memory or HTTP actually live.
type Kit interface {
	Recall(query string, limit int) ([]string, error)
	Remember(fact string) error
	Notify(title, body string) error
	Ask(ctx context.Context, prompt string) (string, error)
	HTTP(ctx context.Context, method, url string, headers map[string]string, body string) (string, int, error)
}

// Runner executes one loop's module.
type Runner struct {
	name      string
	namespace string
	manifest  Manifest
	kit       Kit
	grants    *broker.Handle
	log       *zap.Logger

	declared map[string]bool
	hosts    []string // http allowlist derived from capabilities

	mu       sync.Mutex
	runtime  wazero.Runtime
	compiled wazero.CompiledModule
}

// Options configure a Runner.
type Options struct {
	Namespace string
	Kit       Kit
	Grants    *broker.Handle
	Log       *zap.Logger
	// CacheDir persists compiled modules. Compilation is the expensive step —
	// 650ms cold against 26ms cached in the spike — so it is paid at install.
	CacheDir string
}

// NewRunner compiles a verified artifact.
//
// It takes an Artifact rather than bytes, because an Artifact is what Verify
// produces, and there is no way to reach this with something unverified except
// by constructing one deliberately.
func NewRunner(ctx context.Context, a *Artifact, opts Options) (*Runner, error) {
	// Re-checked here, not only at install: this is the last point before the
	// bytes are compiled, and disk is not a trusted store.
	if got := a.Digest(); got != strings.ToLower(a.Manifest.SHA256) {
		return nil, fmt.Errorf("wasmloop: %s has been modified on disk since it was installed", a.Manifest.Name)
	}

	r := &Runner{
		name: a.Manifest.Name, namespace: opts.Namespace, manifest: a.Manifest,
		kit: opts.Kit, grants: opts.Grants, log: opts.Log,
		declared: set(a.Manifest.Host),
	}
	for _, c := range a.Manifest.Capabilities {
		if host, ok := strings.CutPrefix(c, "http:"); ok {
			r.hosts = append(r.hosts, strings.ToLower(host))
		}
	}

	cfg := wazero.NewRuntimeConfig().WithCloseOnContextDone(true)
	if mb := a.Manifest.MemoryMB; mb > 0 {
		// One page is 64KiB. A runaway loop must not eat the host.
		cfg = cfg.WithMemoryLimitPages(uint32(mb * 16))
	} else {
		cfg = cfg.WithMemoryLimitPages(64 * 16) // 64MB default
	}
	if opts.CacheDir != "" {
		cache, err := wazero.NewCompilationCacheWithDir(opts.CacheDir)
		if err != nil {
			return nil, fmt.Errorf("wasmloop: compilation cache: %w", err)
		}
		cfg = cfg.WithCompilationCache(cache)
	}

	rt := wazero.NewRuntimeWithConfig(ctx, cfg)

	// WASI is instantiated because a Go-compiled guest cannot initialise
	// without it — but the ambient authority comes from the ModuleConfig, not
	// from WASI's presence. The config below grants no filesystem, no
	// environment and no arguments, so path_open has nothing to open and
	// environ_get returns an empty set. Sockets are not in preview1 at all.
	if _, err := wasi_snapshot_preview1.Instantiate(ctx, rt); err != nil {
		rt.Close(ctx)
		return nil, fmt.Errorf("wasmloop: %w", err)
	}
	if err := r.exportHost(ctx, rt); err != nil {
		rt.Close(ctx)
		return nil, err
	}

	compiled, err := rt.CompileModule(ctx, a.Module)
	if err != nil {
		rt.Close(ctx)
		return nil, fmt.Errorf("wasmloop: %s will not compile: %w", a.Manifest.Name, err)
	}
	r.runtime, r.compiled = rt, compiled
	return r, nil
}

// Close releases the runtime.
func (r *Runner) Close(ctx context.Context) error {
	if r.runtime == nil {
		return nil
	}
	return r.runtime.Close(ctx)
}

// Run instantiates the module and calls its entry point.
//
// A fresh instance per run: the spike put that at 3.4ms, which is a good price
// for a loop that cannot carry state or corruption from the last one into the
// next.
func (r *Runner) Run(ctx context.Context, timeout time.Duration) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cfg := wazero.NewModuleConfig().
		WithName("").
		WithStartFunctions("_initialize").
		// Named explicitly so the absence is a decision rather than a default
		// somebody might change: no filesystem, no environment, no arguments.
		WithStdout(logWriter{r.log, r.name, "stdout"}).
		WithStderr(logWriter{r.log, r.name, "stderr"}).
		WithSysNanotime()

	mod, err := r.runtime.InstantiateModule(runCtx, r.compiled, cfg)
	if err != nil {
		return fmt.Errorf("wasmloop: %s would not start: %w", r.name, err)
	}
	defer mod.Close(runCtx)

	fn := mod.ExportedFunction("run")
	if fn == nil {
		return fmt.Errorf("wasmloop: %s exports no run function", r.name)
	}
	if _, err := fn.Call(runCtx); err != nil {
		if runCtx.Err() != nil {
			return fmt.Errorf("wasmloop: %s ran past its %s limit and was stopped", r.name, timeout)
		}
		return fmt.Errorf("wasmloop: %s failed: %w", r.name, err)
	}
	return nil
}

// exportHost wires the single import.
func (r *Runner) exportHost(ctx context.Context, rt wazero.Runtime) error {
	_, err := rt.NewHostModuleBuilder("karmax").
		NewFunctionBuilder().
		WithFunc(func(ctx context.Context, m api.Module,
			namePtr, nameLen, reqPtr, reqLen, outPtr, outCap uint32) int32 {
			return r.call(ctx, m, namePtr, nameLen, reqPtr, reqLen, outPtr, outCap)
		}).
		Export("call").
		Instantiate(ctx)
	return err
}

func (r *Runner) call(ctx context.Context, m api.Module,
	namePtr, nameLen, reqPtr, reqLen, outPtr, outCap uint32) int32 {

	name, ok := readString(m, namePtr, nameLen)
	if !ok {
		return errBadRequest
	}
	// Declared in the signed manifest, or it does not exist for this module.
	// Refused, not logged — a capability system that logs violations and
	// proceeds is a logging system.
	if !r.declared[name] {
		r.log.Warn("wasm loop called a host function it did not declare",
			zap.String("loop", r.name), zap.String("function", name))
		return errNotDeclared
	}
	if _, known := hostDescriptions[name]; !known {
		return errNotDeclared
	}
	if capFor, ok := capabilityFor[name]; ok {
		class, value := capFor(r)
		if err := r.grants.Tool(class + ":" + value); err != nil {
			r.log.Warn("wasm loop was refused a capability",
				zap.String("loop", r.name), zap.String("function", name), zap.Error(err))
			return errNotPermitted
		}
	}

	req, ok := readString(m, reqPtr, reqLen)
	if !ok {
		return errBadRequest
	}

	out, err := r.dispatch(ctx, name, req)
	if err != nil {
		r.log.Warn("wasm host call failed",
			zap.String("loop", r.name), zap.String("function", name), zap.Error(err))
		return errGeneric
	}
	if uint32(len(out)) > outCap {
		// The guest sizes its own buffer, so telling it the size it needs is
		// more useful than truncating something it will then misparse.
		return errTooSmall
	}
	if !m.Memory().Write(outPtr, out) {
		return errGeneric
	}
	return int32(len(out))
}

// dispatch performs one host call. Requests and responses are JSON, which the
// spike showed is nowhere near the cost at this call frequency.
func (r *Runner) dispatch(ctx context.Context, name, req string) ([]byte, error) {
	switch name {
	case FnLog:
		r.log.Info("wasm loop", zap.String("loop", r.name), zap.String("message", trunc(req, 2000)))
		return nil, nil

	case FnTrigger:
		return json.Marshal(map[string]any{"loop": r.name})

	case FnRecall:
		var in struct {
			Query string `json:"query"`
			Limit int    `json:"limit"`
		}
		if err := json.Unmarshal([]byte(req), &in); err != nil {
			return nil, err
		}
		if in.Limit <= 0 {
			in.Limit = 5
		}
		hits, err := r.kit.Recall(in.Query, in.Limit)
		if err != nil {
			return nil, err
		}
		return json.Marshal(map[string]any{"hits": hits})

	case FnRemember:
		var in struct {
			Fact string `json:"fact"`
		}
		if err := json.Unmarshal([]byte(req), &in); err != nil {
			return nil, err
		}
		return nil, r.kit.Remember(in.Fact)

	case FnNotify:
		var in struct {
			Title string `json:"title"`
			Body  string `json:"body"`
		}
		if err := json.Unmarshal([]byte(req), &in); err != nil {
			return nil, err
		}
		return nil, r.kit.Notify(in.Title, in.Body)

	case FnAsk:
		var in struct {
			Prompt string `json:"prompt"`
		}
		if err := json.Unmarshal([]byte(req), &in); err != nil {
			return nil, err
		}
		answer, err := r.kit.Ask(ctx, in.Prompt)
		if err != nil {
			return nil, err
		}
		return json.Marshal(map[string]any{"answer": answer})

	case FnHTTP:
		return r.doHTTP(ctx, req)
	}
	return nil, fmt.Errorf("no host function %q", name)
}

// doHTTP is the egress path, and it passes three gates.
func (r *Runner) doHTTP(ctx context.Context, req string) ([]byte, error) {
	var in struct {
		Method  string            `json:"method"`
		URL     string            `json:"url"`
		Headers map[string]string `json:"headers"`
		Body    string            `json:"body"`
	}
	if err := json.Unmarshal([]byte(req), &in); err != nil {
		return nil, err
	}
	if in.Method == "" {
		in.Method = "GET"
	}

	// 1. Should any loop reach that address — private ranges, metadata, DNS
	//    rebinding.
	if err := safety.CheckURL(in.URL); err != nil {
		return nil, err
	}
	u, err := url.Parse(in.URL)
	if err != nil {
		return nil, err
	}
	host := strings.ToLower(u.Hostname())

	// 2. Did this module DECLARE that host in its signed manifest.
	if !r.allowsHost(host) {
		return nil, fmt.Errorf("%s is not in %s's declared hosts", host, r.name)
	}
	// 3. Did the operator grant it.
	if err := r.grants.HTTP(host); err != nil {
		return nil, err
	}

	body, status, err := r.kit.HTTP(ctx, in.Method, in.URL, in.Headers, in.Body)
	if err != nil {
		return nil, err
	}
	return json.Marshal(map[string]any{"status": status, "body": body})
}

func (r *Runner) allowsHost(host string) bool {
	for _, h := range r.hosts {
		if h == "*" || h == host {
			return true
		}
		if suffix, ok := strings.CutPrefix(h, "*."); ok && strings.HasSuffix(host, "."+suffix) {
			return true
		}
	}
	return false
}

func readString(m api.Module, ptr, length uint32) (string, bool) {
	if length == 0 {
		return "", true
	}
	if length > maxHeaderBytes {
		return "", false
	}
	b, ok := m.Memory().Read(ptr, length)
	if !ok {
		return "", false
	}
	return string(b), true
}

// logWriter sends a guest's stdout and stderr to the log rather than the
// daemon's, so a noisy module is attributable and cannot forge KARMAX output.
type logWriter struct {
	log    *zap.Logger
	loop   string
	stream string
}

func (w logWriter) Write(p []byte) (int, error) {
	msg := strings.TrimRight(string(p), "\n")
	if msg != "" {
		w.log.Info("wasm loop output",
			zap.String("loop", w.loop), zap.String("stream", w.stream), zap.String("message", trunc(msg, 2000)))
	}
	return len(p), nil
}

func trunc(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}
