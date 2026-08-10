package runtime

import (
	"context"
	"os"
	"path/filepath"
	"strings"

	"github.com/MelloB1989/karmax/internal/broker"
	"github.com/MelloB1989/karmax/internal/wasmloop"
	"github.com/MelloB1989/karmax/pkg/loopkit"
	"go.uber.org/zap"
)

// Signed WASM loops, mounted as ordinary loops.
//
// Each one becomes a loopkit.Loop whose body instantiates its module, so it
// inherits durable runs, single-flight leases, retries and dead-lettering
// without the WASM tier knowing any of that exists — the same trick recipes
// use.

// startWasmLoops loads every verified loop from the lockfile.
func (rt *KarmaxRuntime) startWasmLoops(ctx context.Context) {
	dir := wasmloop.Dir()
	in := &wasmloop.Installer{Dir: dir, Trust: rt.loopTrust()}

	entries, err := in.Installed()
	if err != nil {
		rt.log.Warn("could not read the loop lockfile", zap.Error(err))
		return
	}
	if len(entries) == 0 {
		return
	}

	for _, e := range entries {
		if !e.Enabled {
			continue
		}
		// Verified again on load, against the lockfile rather than only against
		// the file's own manifest — a wholesale replacement would otherwise be
		// self-consistent and pass.
		a, err := in.Load(e.Name)
		if err != nil {
			rt.log.Error("refusing to run an installed loop", zap.String("loop", e.Name), zap.Error(err))
			continue
		}

		subject := broker.LoopSubject(e.Name)
		rt.broker.SetTrust(subject, broker.Registry)

		runner, err := wasmloop.NewRunner(ctx, a, wasmloop.Options{
			Namespace: rt.loopNamespace(),
			Kit:       &wasmKit{rt: rt, loop: e.Name},
			Grants:    rt.broker.For(subject),
			Log:       rt.log,
			CacheDir:  filepath.Join(dir, "cache"),
		})
		if err != nil {
			rt.log.Error("a loop would not load", zap.String("loop", e.Name), zap.Error(err))
			continue
		}
		rt.wasmRunners = append(rt.wasmRunners, runner)

		l := loopkit.Loop{
			Name:    e.Name,
			Webhook: a.Manifest.Webhook,
			Events:  a.Manifest.Events,
			Run: func(c context.Context, _ loopkit.Kit) error {
				return runner.Run(c, loopRunTimeout)
			},
		}
		if a.Manifest.Schedule != "" {
			l.Schedule = loopkit.Cron(a.Manifest.Schedule)
		}
		rt.loopkitLoops[e.Name] = l

		rt.log.Info("signed loop loaded",
			zap.String("loop", e.Name), zap.String("version", e.Version),
			zap.String("trust", string(e.Tier)), zap.Strings("host", a.Manifest.Host))
	}
}

// closeWasmLoops releases the runtimes on shutdown.
func (rt *KarmaxRuntime) closeWasmLoops(ctx context.Context) {
	for _, r := range rt.wasmRunners {
		_ = r.Close(ctx)
	}
	rt.wasmRunners = nil
}

// loopTrust is the operator's configuration for which publishers count.
func (rt *KarmaxRuntime) loopTrust() wasmloop.Trust {
	return wasmloop.Trust{
		Registries:     splitCSV(os.Getenv("KARMAX_LOOP_REGISTRIES")),
		Revoked:        splitCSV(os.Getenv("KARMAX_LOOP_REVOKED")),
		AllowCommunity: strings.EqualFold(os.Getenv("KARMAX_LOOP_ALLOW_COMMUNITY"), "true"),
	}
}

func (rt *KarmaxRuntime) loopNamespace() string {
	if len(rt.cfg.Agents) > 0 && rt.cfg.Agents[0].Memory.Namespace != "" {
		return rt.cfg.Agents[0].Memory.Namespace
	}
	return rt.loopDefaultAgent
}

// wasmKit is what a module's host calls actually reach. Everything here is
// already gated by the Broker before it is invoked.
type wasmKit struct {
	rt   *KarmaxRuntime
	loop string
}

func (w *wasmKit) mem() *loopKit {
	return &loopKit{
		loopName: w.loop, agentID: w.rt.loopDefaultAgent,
		namespace: w.rt.loopNamespace(), rt: w.rt,
		mem: w.rt.memory.For(w.rt.loopDefaultAgent, w.rt.loopNamespace()),
	}
}

func (w *wasmKit) Recall(query string, limit int) ([]string, error) {
	return w.mem().Recall(query, limit)
}

func (w *wasmKit) Remember(fact string) error { return w.mem().Remember(fact) }

func (w *wasmKit) Notify(title, body string) error { return w.mem().Notify(title, body) }

func (w *wasmKit) Ask(ctx context.Context, prompt string) (string, error) {
	return w.mem().Ask(ctx, prompt)
}

func (w *wasmKit) HTTP(ctx context.Context, method, url string, headers map[string]string, body string) (string, int, error) {
	return w.mem().HTTP(ctx, method, url, headers, body)
}
