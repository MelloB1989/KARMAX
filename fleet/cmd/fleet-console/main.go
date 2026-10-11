// fleet-console observes the agents on its host and serves the panel.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/MelloB1989/karmax/fleet/internal/console"
	"github.com/MelloB1989/karmax/fleet/internal/target"
)

func main() { os.Exit(run()) }

func run() int {
	cfgPath := flag.String("config", "", "path to console.yaml")
	version := flag.Bool("version", false, "print the version and exit")
	flag.Parse()
	if *version {
		fmt.Println("fleet-console", console.Version)
		return 0
	}
	if *cfgPath == "" {
		fmt.Fprintln(os.Stderr, "fleet-console: --config is required")
		return 2
	}
	cfg, err := console.LoadConfig(*cfgPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "fleet-console: config:", err)
		return 2
	}
	clientToken, err := console.ReadTokenFile(cfg.ClientTokenFile)
	if err != nil {
		fmt.Fprintln(os.Stderr, "fleet-console: client_token_file:", err)
		return 2
	}
	if _, err := console.ReadTokenFile(cfg.FleetdTokenFile); err != nil {
		fmt.Fprintln(os.Stderr, "fleet-console: fleetd_token_file:", err)
		return 2
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	src := console.NewSources(cfg, target.Exec{}, time.Now)
	terms := console.NewTerms(cfg, nil)
	src.Terminals = terms.Source
	hub := console.NewHub(src)
	go hub.Run(ctx)
	srv := &http.Server{Addr: cfg.Listen, Handler: console.Handler(hub, terms, clientToken, src.Fleetd()),
		ReadHeaderTimeout: 10 * time.Second}
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	fmt.Fprintln(os.Stderr, "fleet-console", console.Version, "listening on", cfg.Listen)
	select {
	case err := <-errc:
		fmt.Fprintln(os.Stderr, "fleet-console:", err)
		return 1
	case <-ctx.Done():
	}
	sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(sctx); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return 1
	}
	return 0
}
