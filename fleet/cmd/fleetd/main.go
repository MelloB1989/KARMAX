// Command fleetd is the agent fleet's session manager and system of record:
// it reconciles every agent every tick, archives and rotates sessions, relays
// messages across hosts, keeps the ledger and receives the agents'
// telemetry. See docs/AGENT-FLEET.md.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/MelloB1989/karmax/fleet/internal/agent"
	"github.com/MelloB1989/karmax/fleet/internal/config"
	"github.com/MelloB1989/karmax/fleet/internal/daemon"
	"github.com/MelloB1989/karmax/fleet/internal/ledger"
	"github.com/MelloB1989/karmax/fleet/internal/otlp"
	"github.com/MelloB1989/karmax/fleet/internal/outbound"
	"github.com/MelloB1989/karmax/fleet/internal/server"
	"github.com/MelloB1989/karmax/fleet/internal/target"
)

func defaultConfig() string {
	if p := os.Getenv("FLEET_CONFIG"); p != "" {
		return p
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".karmax", "fleet", "fleet.yaml")
}

func main() {
	cfgPath := flag.String("config", defaultConfig(), "fleet.yaml")
	once := flag.Bool("once", false, "run one tick and exit")
	flag.Parse()
	log.SetFlags(log.LstdFlags | log.Lmsgprefix)
	log.SetPrefix("fleetd: ")
	if err := run(*cfgPath, *once); err != nil {
		log.Fatal(err)
	}
}

// loadEnv reads the env file, with the process environment taking precedence.
func loadEnv(path string) map[string]string {
	env, err := outbound.ReadEnv(path)
	if err != nil {
		env = map[string]string{}
		if !errors.Is(err, os.ErrNotExist) {
			log.Printf("env file %s: %v", path, err)
		}
	}
	for _, kv := range os.Environ() {
		if k, v, ok := strings.Cut(kv, "="); ok && v != "" {
			env[k] = v
		}
	}
	return env
}

func run(cfgPath string, once bool) error {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return err
	}
	env := loadEnv(cfg.EnvFile)
	tokens := server.Tokens{Full: env[cfg.TokenEnv], Relay: env[cfg.RelayTokenEnv]}
	if tokens.Full == "" && !once {
		return fmt.Errorf("%s is not set in %s: fleetd refuses to serve its API without a token", cfg.TokenEnv, cfg.EnvFile)
	}
	db, err := ledger.Open(filepath.Join(cfg.StateDir, "fleet.db"))
	if err != nil {
		return err
	}
	defer db.Close()

	boxes := map[string]daemon.Box{}
	for _, n := range cfg.AgentNames() {
		a := cfg.Agents[n]
		boxes[n] = &agent.Agent{Name: n, C: target.Container{Exec: cfg.Hosts[a.Host].Exec, Name: a.Container}}
	}
	o := cfg.Orchestrator
	boxes[o.Name] = &agent.Agent{Name: o.Name, C: target.Container{Exec: cfg.Hosts[o.Host].Exec, Name: o.Container}}

	var push daemon.Pusher
	if cfg.Karmax.WebhookURL != "" {
		push = outbound.Webhook{URL: cfg.Karmax.WebhookURL, Secret: env[cfg.Karmax.WebhookSecretEnv]}
	}
	karmax := outbound.Karmax{Bin: cfg.Karmax.Bin, APIURL: cfg.Karmax.APIURL, APIToken: env["KARMAX_API_TOKEN"]}
	var notify daemon.Notifier
	if cfg.Karmax.NotifyOn() {
		notify = karmax
	}
	f, err := daemon.New(cfg, db, func(n string) daemon.Box { return boxes[n] }, push, notify)
	if err != nil {
		return err
	}
	f.Log = log.Printf
	f.HostStats = func(ctx context.Context, host string) ([]ledger.HostSample, error) {
		c := target.Container{Exec: cfg.Hosts[host].Exec}
		out, err := c.Host(ctx, "stats", "--no-stream", "--format", "{{json .}}")
		if err != nil {
			return nil, err
		}
		return outbound.ParseDockerStats(out, time.Now()), nil
	}

	var orchQuota atomic.Pointer[outbound.Quota]
	dash := outbound.Dashboard{DataDir: cfg.Karmax.DataDir, ID: cfg.Karmax.Dashboard}
	tick := func(ctx context.Context) {
		now := time.Now()
		tctx, cancel := context.WithTimeout(ctx, cfg.Thresholds.Tick*3)
		defer cancel()
		f.Tick(tctx, now)
		if q, err := karmax.Quota(tctx); err == nil {
			orchQuota.Store(q)
			_ = db.AddQuota(ledger.Quota{Account: o.Name, At: now, FiveHour: q.FiveHour, FiveResets: q.FiveHourResets,
				SevenDay: q.SevenDay, SevenResets: q.SevenDayResets, Source: "harness"})
		}
		if err := writeDashboard(dash, f, db, orchQuota.Load(), now); err != nil {
			log.Printf("dashboard: %v", err)
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if once {
		tick(ctx)
		return nil
	}

	api := server.Handler(f, tokens, orchQuota.Load)
	var servers []*http.Server
	serve := func(addr string, h http.Handler, what string) {
		if addr == "" {
			return
		}
		s := &http.Server{Addr: addr, Handler: h, ReadHeaderTimeout: 10 * time.Second}
		servers = append(servers, s)
		go func() {
			log.Printf("%s listening on %s", what, addr)
			if err := s.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Printf("%s: %v", what, err)
				stop()
			}
		}()
	}
	serve(cfg.Listen, api, "api")
	serve(cfg.RelayListen, api, "relay api")
	serve(cfg.OTLPListen, otlp.Handler(db, tokens.Relay), "otlp")

	log.Printf("%d agents on %d hosts, tick %s", len(cfg.Agents), len(cfg.Hosts), cfg.Thresholds.Tick)
	tick(ctx)
	t := time.NewTicker(cfg.Thresholds.Tick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			for _, s := range servers {
				_ = s.Shutdown(sctx)
			}
			cancel()
			log.Printf("stopped")
			return nil
		case <-t.C:
			tick(ctx)
		}
	}
}

// writeDashboard rewrites the Fleet dashboard's data files.
func writeDashboard(d outbound.Dashboard, f *daemon.Fleet, db *ledger.DB, orch *outbound.Quota, now time.Time) error {
	if d.ID == "" {
		return nil
	}
	quota, _ := db.LatestQuota()
	usage, _ := db.UsageBy("agent", now.Add(-7*24*time.Hour))
	all, _ := db.Events("", now.Add(-24*time.Hour))
	var events []ledger.Event
	for i := len(all) - 1; i >= 0 && len(events) < 50; i-- {
		if strings.HasPrefix(all[i].Kind, "fleet.") {
			events = append(events, all[i])
		}
	}
	return d.Write(map[string]any{
		"agents":       f.Status(now),
		"orchestrator": orch,
		"quota":        quota,
		"usage":        usage,
		"events":       events,
		"updated":      map[string]any{"at": now.UTC()},
	})
}
