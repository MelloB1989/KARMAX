package server

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/MelloB1989/karmax/fleet/internal/desk"
)

// TestDeskDevServer is the desk's API on a desk of its own, for the window's
// end-to-end runs, never for go test ./...: it runs when
// FLEET_DESK_DEV_ADDR says where to listen, until FLEET_DESK_DEV_MINUTES
// (60) pass or the file FLEET_DESK_DEV_STOP appears. The full token is
// dev-token; the desk's tmux is -L FLEET_DESK_DEV_SOCKET (fleet-desk-dev),
// its state FLEET_DESK_DEV_STATE (a temp dir), its Claude Code
// FLEET_DESK_DEV_CLAUDE (claude).
func TestDeskDevServer(t *testing.T) {
	addr := os.Getenv("FLEET_DESK_DEV_ADDR")
	if addr == "" {
		t.Skip("FLEET_DESK_DEV_ADDR is not set")
	}
	env := func(k, def string) string {
		if v := os.Getenv(k); v != "" {
			return v
		}
		return def
	}
	state := env("FLEET_DESK_DEV_STATE", t.TempDir())
	dk := desk.New(env("FLEET_DESK_DEV_SOCKET", "fleet-desk-dev"), state)
	dk.SystemdRun = ""
	dk.Claude = env("FLEET_DESK_DEV_CLAUDE", "claude")
	dk.PasteDir = filepath.Join(state, "paste")
	dk.Log = t.Logf
	minutes, _ := strconv.Atoi(env("FLEET_DESK_DEV_MINUTES", "60"))
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(minutes)*time.Minute)
	defer cancel()
	go dk.Run(ctx)
	srv := &http.Server{Addr: addr, Handler: Handler(WithDesk(&fake{}, dk), Tokens{Full: "dev-token"}, nil)}
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			t.Error(err)
			cancel()
		}
	}()
	t.Logf("desk dev server on %s, state %s", addr, state)
	stop := os.Getenv("FLEET_DESK_DEV_STOP")
	for ctx.Err() == nil {
		if stop != "" {
			if _, err := os.Stat(stop); err == nil {
				break
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	srv.Close()
}
