package server

import (
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/MelloB1989/karmax/fleet/internal/desk"
	"github.com/MelloB1989/karmax/fleet/internal/hostfiles"
)

// TestDeskDevServer is the desk's API on a desk of its own, for the window's
// end-to-end runs, never for go test ./...: it runs when
// FLEET_DESK_DEV_ADDR says where to listen, until FLEET_DESK_DEV_MINUTES
// (60) pass or the file FLEET_DESK_DEV_STOP appears. The full token is
// dev-token; the desk's tmux is -L FLEET_DESK_DEV_SOCKET (fleet-desk-dev),
// its state FLEET_DESK_DEV_STATE (a temp dir), its Claude Code
// FLEET_DESK_DEV_CLAUDE (claude). Its files are FLEET_FILES_DEV_HOME's (the
// state dir), their bulk routes on the local network at FLEET_FILES_DEV_LAN
// (none when unset).
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
	fm := hostfiles.New(state)
	fm.Home, fm.Log = env("FLEET_FILES_DEV_HOME", state), t.Logf
	var lan *LAN
	if l := os.Getenv("FLEET_FILES_DEV_LAN"); l != "" {
		_, port, _ := net.SplitHostPort(l)
		var err error
		if lan, err = NewLAN(port); err != nil {
			t.Fatal(err)
		}
	}
	ops := WithFiles(WithDesk(&fake{}, dk), HostFiles(fm), lan)
	if lan != nil {
		ls := &http.Server{Handler: lan.Handler(ops), TLSConfig: lan.TLS()}
		ln, err := net.Listen("tcp", os.Getenv("FLEET_FILES_DEV_LAN"))
		if err != nil {
			t.Fatal(err)
		}
		go ls.ServeTLS(ln, "", "")
		defer ls.Close()
	}
	srv := &http.Server{Addr: addr, Handler: Handler(ops, Tokens{Full: "dev-token"}, nil)}
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
