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
	FnLog         = "log"
	FnRecall      = "recall"
	FnRemember    = "remember"
	FnNotify      = "notify"
	FnHTTP        = "http"
	FnTrigger     = "trigger"
	FnAsk         = "ask"
	FnConfig      = "config"
	FnHostTool    = "hosttool"
	FnHarness     = "harness"
	FnGateway     = "gateway"
	FnSummarize   = "summarize"
	FnPropose     = "propose"
	FnRemind      = "remind"
	FnTool        = "tool"
	FnShortSet    = "short_set"
	FnShortGet    = "short_get"
	FnShortAll    = "short_all"
	FnChatGet     = "chat_summary_get"
	FnChatSave    = "chat_summary_save"
	FnRunLoop     = "run_loop"
	FnShortForget = "short_forget"
	FnOperators   = "operator_chats"
)

var hostDescriptions = map[string]string{
	FnLog:         "write to KARMAX's log",
	FnRecall:      "read your long-term memory",
	FnRemember:    "write to your long-term memory",
	FnNotify:      "send you notifications",
	FnHTTP:        "use the network — but only the hosts listed above",
	FnTrigger:     "see what triggered it",
	FnAsk:         "ask your agent questions (which can use its tools)",
	FnConfig:      "read the settings you gave it at install",
	FnHostTool:    "learn where wacli and gws live (a path, not permission to run them)",
	FnHarness:     "run a coding harness — shell, files and web research",
	FnGateway:     "ask the main model directly",
	FnSummarize:   "summarise text with the cheap model",
	FnPropose:     "ask for your approval before acting",
	FnRemind:      "put reminders on your list",
	FnTool:        "call the tools listed above, and nothing else",
	FnShortSet:    "keep short-term working notes",
	FnShortGet:    "read its short-term working notes",
	FnShortAll:    "read all its short-term working notes",
	FnChatGet:     "read stored per-chat summaries",
	FnChatSave:    "write stored per-chat summaries",
	FnRunLoop:     "trigger your other loops",
	FnShortForget: "clear its short-term working notes",
	FnOperators:   "know which chats are yours rather than someone else's",
}

// capabilityFor maps a host function to the Broker capability it needs, so the
// two cannot drift apart.
//
// FnTool is absent deliberately: its capability depends on WHICH tool is being
// called, so it is checked per call in Runner.call rather than per function.
var capabilityFor = map[string]func(*Runner) (class, value string){
	FnRecall:      func(r *Runner) (string, string) { return "memory", r.namespace },
	FnRemember:    func(r *Runner) (string, string) { return "memory", r.namespace + ":write" },
	FnNotify:      func(r *Runner) (string, string) { return "tool", "app.push" },
	FnAsk:         func(r *Runner) (string, string) { return "tool", "agent.ask" },
	FnHarness:     func(r *Runner) (string, string) { return "tool", "harness" },
	FnGateway:     func(r *Runner) (string, string) { return "tool", "gateway" },
	FnSummarize:   func(r *Runner) (string, string) { return "tool", "summarize" },
	FnPropose:     func(r *Runner) (string, string) { return "tool", "propose" },
	FnRemind:      func(r *Runner) (string, string) { return "tool", "reminder.add" },
	FnShortSet:    func(r *Runner) (string, string) { return "memory", r.namespace + ":write" },
	FnShortGet:    func(r *Runner) (string, string) { return "memory", r.namespace },
	FnShortAll:    func(r *Runner) (string, string) { return "memory", r.namespace },
	FnChatGet:     func(r *Runner) (string, string) { return "memory", r.namespace },
	FnChatSave:    func(r *Runner) (string, string) { return "memory", r.namespace + ":write" },
	FnRunLoop:     func(r *Runner) (string, string) { return "tool", "loop.run" },
	FnShortForget: func(r *Runner) (string, string) { return "memory", r.namespace + ":write" },
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

	Config(key string) string
	HostTool(name string) string
	Harness(ctx context.Context, prompt string) (string, error)
	Gateway(ctx context.Context, prompt string, lend ...string) (string, error)
	Summarize(ctx context.Context, prompt string) (string, error)
	Propose(title, summary, action string) error
	Remind(title, due, notes string) error
	// Tool calls one of KARMAX's tools by name. Integrations reach a loop
	// through here and nowhere else, so adding one costs no ABI.
	Tool(ctx context.Context, name string, input map[string]any) (string, error)
	ShortSet(group, key, value string, ttlSeconds int) error
	ShortGet(group, key string) (string, bool, error)
	ShortAll(group string) ([]ShortMemory, error)
	ChatSummary(jid string) (*ChatSummary, error)
	SaveChatSummary(ChatSummary) error
	RunLoop(name string) error
	ShortForget(group, key string) error
	OperatorChats() []string
}

// ShortMemory is one short-term working note.
type ShortMemory struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

// ChatSummary is the stored cold-memory record for one chat.
type ChatSummary struct {
	ChatJID         string `json:"jid"`
	ChatName        string `json:"name"`
	IsGroup         bool   `json:"is_group"`
	Summary         string `json:"summary"`
	MessageCount    int    `json:"message_count"`
	OwnMessageCount int    `json:"own_count"`
	// Unix seconds, because a guest and a host do not share a time.Time.
	LastMessageAt int64  `json:"last_message_at"`
	SummarizedAt  int64  `json:"summarized_at"`
	Status        string `json:"status"`
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
	// tools is the manifest's tool allowlist, the first of the two gates a
	// tool call passes. The Broker is the second.
	tools map[string]bool
	hosts []string // http allowlist derived from capabilities

	// trigger is what started the current run, readable by the guest.
	mu2         sync.Mutex
	trigger     map[string]any
	triggerKind string

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
		tools:    set(a.Manifest.Tools),
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
// Run executes the module for one trigger.
func (r *Runner) Run(ctx context.Context, timeout time.Duration) error {
	return r.RunTriggered(ctx, timeout, "manual", nil)
}

// RunTriggered is Run with the trigger the guest can read back.
func (r *Runner) RunTriggered(ctx context.Context, timeout time.Duration, kind string, payload map[string]any) error {
	r.mu2.Lock()
	r.trigger, r.triggerKind = payload, kind
	r.mu2.Unlock()
	return r.run(ctx, timeout)
}

func (r *Runner) run(ctx context.Context, timeout time.Duration) error {
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
		if err := r.grants.Check(class, value); err != nil {
			r.log.Warn("wasm loop was refused a capability",
				zap.String("loop", r.name), zap.String("function", name), zap.Error(err))
			return errNotPermitted
		}
	}

	req, ok := readString(m, reqPtr, reqLen)
	if !ok {
		return errBadRequest
	}

	// A tool call carries its own subject, so its gates are here rather than in
	// capabilityFor: the manifest's tool list first, then the Broker.
	if name == FnTool {
		tool, err := toolName(req)
		if err != nil {
			return errBadRequest
		}
		if !r.tools[tool] {
			r.log.Warn("wasm loop called a tool it did not declare",
				zap.String("loop", r.name), zap.String("tool", tool))
			return errNotDeclared
		}
		if err := r.grants.Tool(tool); err != nil {
			r.log.Warn("wasm loop was refused a tool",
				zap.String("loop", r.name), zap.String("tool", tool), zap.Error(err))
			return errNotPermitted
		}
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
		r.mu2.Lock()
		t := r.trigger
		r.mu2.Unlock()
		if t == nil {
			t = map[string]any{}
		}
		return json.Marshal(map[string]any{"loop": r.name, "kind": r.triggerKind, "payload": t})

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

	case FnConfig:
		return json.Marshal(map[string]any{"value": r.kit.Config(req)})

	case FnHostTool:
		// A path, not permission to run it. A sandboxed guest cannot exec;
		// this exists so a loop can name the binary inside a harness prompt,
		// and the harness — host-side — is what actually runs it.
		return json.Marshal(map[string]any{"value": r.kit.HostTool(req)})

	case FnHarness, FnGateway, FnSummarize:
		var in struct {
			Prompt string   `json:"prompt"`
			Lend   []string `json:"lend"`
		}
		if err := json.Unmarshal([]byte(req), &in); err != nil {
			return nil, err
		}
		var (
			answer string
			err    error
		)
		switch name {
		case FnHarness:
			answer, err = r.kit.Harness(ctx, in.Prompt)
		case FnGateway:
			// Lent tools are named, not supplied. A compiled-in loop passed a
			// Go closure that shelled out; a guest cannot, and should not — the
			// read-only allowlist belongs to the host, where it is one rule
			// rather than one per loop.
			answer, err = r.kit.Gateway(ctx, in.Prompt, in.Lend...)
		default:
			answer, err = r.kit.Summarize(ctx, in.Prompt)
		}
		if err != nil {
			return nil, err
		}
		return json.Marshal(map[string]any{"answer": answer})

	case FnPropose:
		var in struct {
			Title, Summary, Action string
		}
		if err := json.Unmarshal([]byte(req), &in); err != nil {
			return nil, err
		}
		return nil, r.kit.Propose(in.Title, in.Summary, in.Action)

	case FnRemind:
		var in struct {
			Title, Due, Notes string
		}
		if err := json.Unmarshal([]byte(req), &in); err != nil {
			return nil, err
		}
		return nil, r.kit.Remind(in.Title, in.Due, in.Notes)

	case FnTool:
		var in struct {
			Name  string         `json:"name"`
			Input map[string]any `json:"input"`
		}
		if err := json.Unmarshal([]byte(req), &in); err != nil {
			return nil, err
		}
		// The error travels in the payload rather than being returned, so the
		// guest still receives the output. For an integration the output IS
		// often the diagnosis — an auth failure's JSON is what lets a loop say
		// "run gws auth login" instead of "it failed".
		out, err := r.kit.Tool(ctx, in.Name, in.Input)
		return json.Marshal(map[string]any{"output": out, "error": errText(err)})

	case FnShortSet:
		var in struct {
			Group, Key, Value string
			TTLSeconds        int `json:"ttl_seconds"`
		}
		if err := json.Unmarshal([]byte(req), &in); err != nil {
			return nil, err
		}
		return nil, r.kit.ShortSet(in.Group, in.Key, in.Value, in.TTLSeconds)

	case FnShortGet:
		var in struct{ Group, Key string }
		if err := json.Unmarshal([]byte(req), &in); err != nil {
			return nil, err
		}
		v, found, err := r.kit.ShortGet(in.Group, in.Key)
		if err != nil {
			return nil, err
		}
		return json.Marshal(map[string]any{"value": v, "found": found})

	case FnShortAll:
		var in struct{ Group string }
		if err := json.Unmarshal([]byte(req), &in); err != nil {
			return nil, err
		}
		all, err := r.kit.ShortAll(in.Group)
		if err != nil {
			return nil, err
		}
		return json.Marshal(map[string]any{"entries": all})

	case FnChatGet:
		var in struct{ JID string }
		if err := json.Unmarshal([]byte(req), &in); err != nil {
			return nil, err
		}
		rec, err := r.kit.ChatSummary(in.JID)
		if err != nil {
			return nil, err
		}
		return json.Marshal(map[string]any{"found": rec != nil, "summary": rec})

	case FnChatSave:
		var in ChatSummary
		if err := json.Unmarshal([]byte(req), &in); err != nil {
			return nil, err
		}
		return nil, r.kit.SaveChatSummary(in)

	case FnRunLoop:
		return nil, r.kit.RunLoop(req)

	case FnShortForget:
		var in struct{ Group, Key string }
		if err := json.Unmarshal([]byte(req), &in); err != nil {
			return nil, err
		}
		return nil, r.kit.ShortForget(in.Group, in.Key)

	case FnOperators:
		// Read from the host rather than the environment. A loop that needs to
		// tell the operator's own chats from a third party's used to call
		// os.Getenv, which the sandbox removed — and having it explicit is
		// better than having it ambient, since the daemon's environment holds
		// rather more than this.
		return json.Marshal(map[string]any{"chats": r.kit.OperatorChats()})
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

// toolName pulls the tool being called out of a request, so the gates can run
// before the request reaches dispatch and the Kit.
func toolName(req string) (string, error) {
	var in struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal([]byte(req), &in); err != nil {
		return "", err
	}
	if in.Name = strings.TrimSpace(in.Name); in.Name == "" {
		return "", fmt.Errorf("wasmloop: a tool call named no tool")
	}
	return in.Name, nil
}

// errText renders an error for the guest, empty when there was none.
func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func trunc(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}
