package main

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MelloB1989/karmax/fleet/internal/filexfer"
	"github.com/MelloB1989/karmax/fleet/internal/fleetapi"
	"github.com/MelloB1989/karmax/fleet/internal/hostfiles"
	"github.com/MelloB1989/karmax/fleet/internal/server"
)

// opsOnlyFiles is fleetd with nothing but its files.
type opsOnlyFiles struct{ server.Ops }

// filesRig is fleetd's API and its LAN listener, both on loopback, its
// files a test's home; counts is the requests each one took.
func filesRig(t *testing.T) (a *apiClient, home string, apiHits, lanHits *atomic.Int64, lanSrv *httptest.Server) {
	t.Helper()
	root := t.TempDir()
	home = filepath.Join(root, "home")
	os.MkdirAll(home, 0o755)
	os.WriteFile(filepath.Join(home, "big.bin"), []byte(strings.Repeat("x", 300_000)), 0o644)
	m := &hostfiles.Manager{Home: home, State: filepath.Join(root, "state"), Host: "kali"}
	lan, err := server.NewLAN("0")
	if err != nil {
		t.Fatal(err)
	}
	ops := server.WithFiles(opsOnlyFiles{}, server.HostFiles(m), lan)
	apiHits, lanHits = new(atomic.Int64), new(atomic.Int64)
	h := server.Handler(ops, server.Tokens{Full: "tok"}, nil)
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { apiHits.Add(1); h.ServeHTTP(w, r) }))
	t.Cleanup(api.Close)
	lh := lan.Handler(ops)
	lanSrv = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { lanHits.Add(1); lh.ServeHTTP(w, r) }))
	lanSrv.TLS = lan.TLS()
	lanSrv.StartTLS()
	t.Cleanup(lanSrv.Close)
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(lanSrv.URL, "https://"))
	lan.Port = port
	lan.Addrs = func() []string { return []string{"192.0.2.1", "127.0.0.1"} } // one that never answers, one that does
	old := sameMachine
	sameMachine = func(string) bool { return false }
	t.Cleanup(func() { sameMachine = old; lanState = lanRoute{} })
	lanState = lanRoute{}
	return &apiClient{c: &client{base: api.URL, token: "tok"}}, home, apiHits, lanHits, lanSrv
}

// A client on fleetd's local network moves files there, not on the
// tailnet; when the local network goes, the tailnet takes them at once.
func TestFilesMoveOverTheLocalNetworkWhenThereIsOne(t *testing.T) {
	a, home, apiHits, lanHits, lanSrv := filesRig(t)
	ctx := context.Background()
	d, err := a.FilesList(ctx, "~")
	if err != nil || d.Path != home || d.Host != "kali" {
		t.Fatalf("list: %+v %v", d, err)
	}
	start := time.Now()
	r, err := a.FilesRead(ctx, "~/big.bin", 1000, -1, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(r.Body)
	r.Body.Close()
	if r.Route != fleetapi.RouteLAN || r.From != 1000 || r.Size != 300_000 || len(b) != 299_000 {
		t.Fatalf("read by %s from %d of %d: %d bytes", r.Route, r.From, r.Size, len(b))
	}
	if time.Since(start) > 3*time.Second {
		t.Fatalf("finding the LAN took %s: an address that does not answer held it up", time.Since(start))
	}
	if a.FilesRoute() != fleetapi.RouteLAN || lanHits.Load() < 2 {
		t.Fatalf("route %s, %d LAN requests", a.FilesRoute(), lanHits.Load())
	}
	before := apiHits.Load()
	id := hostfiles.NewUploadID()
	p, err := a.FilesPut(ctx, "~", "up.txt", id, 0, 5, strings.NewReader("hello"))
	if err != nil || !p.Done || apiHits.Load() != before {
		t.Fatalf("an upload: %+v %v (%d calls on the tailnet)", p, err, apiHits.Load()-before)
	}
	// The laptop leaves home: the next read goes by the tailnet.
	lanSrv.Close()
	r, err = a.FilesRead(ctx, "~/up.txt", 0, -1, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	b, _ = io.ReadAll(r.Body)
	r.Body.Close()
	if r.Route != fleetapi.RouteTailnet || string(b) != "hello" {
		t.Fatalf("after the LAN went: %s %q", r.Route, b)
	}
	if a.FilesRoute() != fleetapi.RouteTailnet {
		t.Fatalf("route %s", a.FilesRoute())
	}
}

func TestATarComesUngzippedWhateverTheRoute(t *testing.T) {
	a, home, _, _, lanSrv := filesRig(t)
	os.MkdirAll(filepath.Join(home, "dir"), 0o755)
	os.WriteFile(filepath.Join(home, "dir", "a.txt"), []byte("aaa"), 0o644)
	lanSrv.Close() // the tailnet: gzipped on the way
	lanState = lanRoute{looked: time.Now()}
	r, err := a.FilesTar(context.Background(), "~/dir")
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	dst := t.TempDir()
	if err := extractTo(dst, r.Body); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(dst, "dir", "a.txt")); string(b) != "aaa" || r.Size != 3 || r.Route != fleetapi.RouteTailnet {
		t.Fatalf("%q, size %d, by %s", b, r.Size, r.Route)
	}
}

func extractTo(dir string, r io.Reader) error { return filexfer.Extract(dir, r) }
