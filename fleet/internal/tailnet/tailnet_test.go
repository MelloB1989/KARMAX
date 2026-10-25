package tailnet

import (
	"context"
	"errors"
	"net"
	"net/http"
	"path/filepath"
	"sync/atomic"
	"testing"
)

// tailscaled, as far as WhoIs goes: two people's laptops, a subnet router.
func fakeTailscaled(t *testing.T) (string, *atomic.Int32) {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "tailscaled.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path != "/localapi/v0/whois" || r.Host != "local-tailscaled.sock" {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		switch r.URL.Query().Get("addr") {
		case "100.64.0.2:50123", "100.64.0.2:50124":
			_, _ = w.Write([]byte(`{"Node":{"Name":"ram-laptop.tailnet-example.ts.net.","StableID":"nRAM1CNTRL","Tags":null},
				"UserProfile":{"LoginName":"ram@example.com","DisplayName":"Ram P"}}`))
		case "100.64.0.5:4000":
			_, _ = w.Write([]byte(`{"Node":{"Name":"router.tailnet-example.ts.net.","Tags":["tag:app-connector"]},
				"UserProfile":{"LoginName":"tagged-devices","DisplayName":"Tagged Devices"}}`))
		default:
			http.Error(w, "no match for IP:port", http.StatusNotFound)
		}
	})}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return sock, &calls
}

func TestWhoIsNamesThePersonBehindAnAddress(t *testing.T) {
	sock, calls := fakeTailscaled(t)
	l := &Local{Socket: sock}
	w, err := l.WhoIs(context.Background(), "100.64.0.2:50123")
	if err != nil || w != (Who{Login: "ram@example.com", Name: "Ram P", Node: "ram-laptop.tailnet-example.ts.net", StableID: "nRAM1CNTRL"}) {
		t.Fatalf("who %+v err %v", w, err)
	}
	// The same machine again, another connection: remembered, not asked.
	if w2, err := l.WhoIs(context.Background(), "100.64.0.2:50124"); err != nil || w2 != w || calls.Load() != 1 {
		t.Fatalf("again: %+v %v, %d calls", w2, err, calls.Load())
	}
}

// A tagged device is nobody: a subnet router's traffic can be anyone's.
func TestATaggedDeviceHasNoLogin(t *testing.T) {
	sock, _ := fakeTailscaled(t)
	w, err := (&Local{Socket: sock}).WhoIs(context.Background(), "100.64.0.5:4000")
	if err != nil || w.Login != "" || w.Node != "router.tailnet-example.ts.net" {
		t.Fatalf("who %+v err %v", w, err)
	}
}

func TestAnAddressOffTheTailnetIsNotAPeer(t *testing.T) {
	sock, _ := fakeTailscaled(t)
	if _, err := (&Local{Socket: sock}).WhoIs(context.Background(), "192.0.2.26:5000"); !errors.Is(err, ErrNotPeer) {
		t.Fatalf("err %v", err)
	}
}

func TestNoTailscaledIsAnError(t *testing.T) {
	if _, err := (&Local{Socket: filepath.Join(t.TempDir(), "none.sock")}).WhoIs(context.Background(), "100.64.0.2:1"); err == nil || errors.Is(err, ErrNotPeer) {
		t.Fatalf("err %v", err)
	}
}
