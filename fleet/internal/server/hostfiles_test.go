package server

import (
	"compress/gzip"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MelloB1989/karmax/fleet/internal/config"
	"github.com/MelloB1989/karmax/fleet/internal/fleetapi"
	"github.com/MelloB1989/karmax/fleet/internal/hostfiles"
)

// hostSetup is the API with fleetd's machine's files in a test's own home,
// on the LAN too.
func hostSetup(t *testing.T) (home string, api, tn http.Handler, lan *LAN, ops Ops) {
	t.Helper()
	root := t.TempDir()
	home = filepath.Join(root, "home")
	os.MkdirAll(filepath.Join(home, "code"), 0o755)
	os.WriteFile(filepath.Join(home, "code", "notes.txt"), []byte("0123456789"), 0o644)
	m := &hostfiles.Manager{Home: home, State: filepath.Join(root, "state"), Host: "kali"}
	lan, err := NewLAN("7877")
	if err != nil {
		t.Fatal(err)
	}
	lan.Addrs = func() []string { return []string{"192.168.1.20"} }
	ops = WithFiles(WithDesk(&fake{}, &fakeDesk{}), HostFiles(m), lan)
	api = Handler(ops, Tokens{Full: "full", Relay: "relay", Console: "console"}, nil)
	tn = Tailnet(api, tailnetWho, tagDevices(t), &config.Tailnet{Listen: "100.64.0.1:7878", GuestOpen: 2, Dist: t.TempDir()})
	return home, api, tn, lan, ops
}

func TestFleetdsFilesAreTheOperatorsAlone(t *testing.T) {
	_, api, tn, _, _ := hostSetup(t)
	routes := []struct{ method, path, body string }{
		{"GET", "/v1/files?path=~", ""},
		{"GET", "/v1/files/read?path=~/code/notes.txt", ""},
		{"GET", "/v1/files/tar?path=~/code", ""},
		{"PUT", "/v1/files/put?dir=~&name=x&id=abcdefgh12&offset=0&size=1", "x"},
		{"GET", "/v1/files/put?id=abcdefgh12", ""},
		{"POST", "/v1/files/op", `{"op":"mkdir","path":"~/made"}`},
		{"GET", "/v1/files/lan", ""},
	}
	for _, r := range routes {
		for tok, want := range map[string]int{"relay": 403, "console": 403, "": 401} {
			if rec := do(t, api, r.method, r.path, tok, r.body); rec.Code != want {
				t.Errorf("%s %s as %q: %d %s, want %d", r.method, r.path, tok, rec.Code, rec.Body, want)
			}
		}
		for _, ip := range []string{ramIP, friendIP} {
			if rec := tdo(t, tn, r.method, r.path, ip, "", r.body); rec.Code != 403 {
				t.Errorf("%s %s as a guest: %d %s, want 403", r.method, r.path, rec.Code, rec.Body)
			}
		}
		if rec := tdo(t, tn, r.method, r.path, opIP, "", r.body); rec.Code != 200 {
			t.Errorf("%s %s from an admin device: %d %s", r.method, r.path, rec.Code, rec.Body)
		}
	}
}

func TestFleetdsFilesRoutes(t *testing.T) {
	home, api, _, _, _ := hostSetup(t)
	// A big folder is gzipped on the way.
	for i := range 300 {
		os.WriteFile(filepath.Join(home, "code", fmt.Sprintf("a-rather-long-file-name-for-a-listing-%03d.txt", i)), nil, 0o644)
	}
	req := httptest.NewRequest("GET", "/v1/files?path=~/code", nil)
	req.Header.Set("Authorization", "Bearer full")
	req.Header.Set("Accept-Encoding", "gzip")
	rec := httptest.NewRecorder()
	api.ServeHTTP(rec, req)
	if rec.Code != 200 || rec.Header().Get("Content-Encoding") != "gzip" {
		t.Fatalf("a big folder: %d %v", rec.Code, rec.Header())
	}
	z, _ := gzip.NewReader(rec.Body)
	var d fleetapi.FileDir
	if err := json.NewDecoder(z).Decode(&d); err != nil || d.Path != filepath.Join(home, "code") || len(d.Entries) != 301 || d.Host != "kali" {
		t.Fatalf("listed %d entries of %s: %v", len(d.Entries), d.Path, err)
	}
	// A read takes a range.
	req = httptest.NewRequest("GET", "/v1/files/read?path=~/code/notes.txt", nil)
	req.Header.Set("Authorization", "Bearer full")
	req.Header.Set("Range", "bytes=4-")
	rec = httptest.NewRecorder()
	api.ServeHTTP(rec, req)
	if rec.Code != 206 || rec.Body.String() != "456789" || rec.Header().Get("Content-Range") != "bytes 4-9/10" {
		t.Fatalf("a range: %d %q %v", rec.Code, rec.Body, rec.Header())
	}
	for _, c := range []struct {
		method, path, body string
		want               int
	}{
		{"GET", "/v1/files?path=~/nope", "", 404},
		{"GET", "/v1/files/read?path=~/code", "", 400},
		{"POST", "/v1/files/op", `{"op":"mkdir","path":"~/code"}`, 409},
		{"POST", "/v1/files/op", `{"op":"trash","paths":["~"]}`, 400},
		{"POST", "/v1/files/op", `{"op":"explode"}`, 400},
		{"PUT", "/v1/files/put?dir=~&name=../x&id=abcdefgh12&offset=0&size=1", "x", 400},
		{"PUT", "/v1/files/put?dir=~&name=x&id=abcdefgh13&offset=5&size=9", "x", 409},
		{"GET", "/v1/files/preview?path=~/code/notes.txt&w=10&h=10", "", 400},
	} {
		if rec := do(t, api, c.method, c.path, "full", c.body); rec.Code != c.want {
			t.Errorf("%s %s %s: %d %s, want %d", c.method, c.path, c.body, rec.Code, rec.Body, c.want)
		}
	}
	// An upload in two parts, then the folder as a tar and back.
	if rec := do(t, api, "PUT", "/v1/files/put?dir=~/code&name=up.txt&id=abcdefgh14&offset=0&size=6", "full", "abc"); rec.Code != 400 {
		t.Fatalf("a short part: %d %s", rec.Code, rec.Body)
	}
	if rec := do(t, api, "GET", "/v1/files/put?id=abcdefgh14", "full", ""); !strings.Contains(rec.Body.String(), `"have":3`) {
		t.Fatalf("held: %s", rec.Body)
	}
	rec = do(t, api, "PUT", "/v1/files/put?dir=~/code&name=up.txt&id=abcdefgh14&offset=3&size=6", "full", "def")
	if b, _ := os.ReadFile(filepath.Join(home, "code", "up.txt")); rec.Code != 200 || string(b) != "abcdef" {
		t.Fatalf("the rest: %d %s %q", rec.Code, rec.Body, b)
	}
	rec = do(t, api, "GET", "/v1/files/tar?path=~/code&gz=1", "full", "")
	if rec.Code != 200 || rec.Header().Get("X-Fleet-Bytes") != "16" {
		t.Fatalf("tar: %d %v", rec.Code, rec.Header())
	}
	tarball := rec.Body.Bytes()
	os.MkdirAll(filepath.Join(home, "copy"), 0o755)
	rec = do(t, api, "PUT", "/v1/files/untar?dir=~/copy", "full", string(tarball))
	if b, _ := os.ReadFile(filepath.Join(home, "copy", "code", "up.txt")); rec.Code != 200 || string(b) != "abcdef" {
		t.Fatalf("untar: %d %s", rec.Code, rec.Body)
	}
}

// The local network takes the bytes, with the token the API gave, over
// TLS whose certificate is the one the API named; nothing else.
func TestTheLANCarriesFilesForTheTokensHolderAlone(t *testing.T) {
	_, api, _, lan, ops := hostSetup(t)
	rec := do(t, api, "GET", "/v1/files/lan", "full", "")
	var l fleetapi.FileLAN
	if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &l) != nil || len(l.Addrs) != 1 || l.Addrs[0] != "192.168.1.20:7877" || l.Token == "" || len(l.Pin) != 64 {
		t.Fatalf("lan: %d %s", rec.Code, rec.Body)
	}
	srv := httptest.NewUnstartedServer(lan.Handler(ops))
	srv.TLS = lan.TLS()
	srv.StartTLS()
	defer srv.Close()
	var seen []byte
	hc := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true,
		VerifyConnection: func(cs tls.ConnectionState) error {
			sum := sha256.Sum256(cs.PeerCertificates[0].Raw)
			seen = sum[:]
			return nil
		}}}}
	get := func(path, tok string) (int, string) {
		req, _ := http.NewRequest("GET", srv.URL+path, nil)
		if tok != "" {
			req.Header.Set("Authorization", "Bearer "+tok)
		}
		resp, err := hc.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	if code, body := get("/v1/files/read?path=~/code/notes.txt", l.Token); code != 200 || body != "0123456789" {
		t.Fatalf("a read on the LAN: %d %q", code, body)
	}
	if hex.EncodeToString(seen) != l.Pin {
		t.Fatal("the LAN's certificate is not the one the API named")
	}
	for _, tok := range []string{"", "full", strings.Repeat("0", 64)} {
		if code, _ := get("/v1/files/read?path=~/code/notes.txt", tok); code != 401 {
			t.Errorf("token %q: %d, want 401", tok, code)
		}
	}
	// Only what moves bytes: no listing, no changes, no other route.
	for _, p := range []string{"/v1/files?path=~", "/v1/status", "/v1/files/lan", "/v1/desk"} {
		if code, _ := get(p, l.Token); code != 404 && code != 405 {
			t.Errorf("%s on the LAN: %d", p, code)
		}
	}
	req, _ := http.NewRequest("GET", srv.URL+"/v1/files/read?path=~/code/notes.txt", nil)
	req.Header.Set("Authorization", "Bearer "+l.Token)
	req.Header.Set("Origin", "http://evil.example")
	if resp, err := hc.Do(req); err != nil || resp.StatusCode != 403 {
		t.Fatalf("a page's request on the LAN: %v %v", resp, err)
	}
}

func TestLANAddrsLeaveOutTheTailnetAndContainers(t *testing.T) {
	for name, want := range map[string]bool{"docker0": true, "br-61adf5efd455": true, "tailscale0": true, "veth12": true,
		"fleet-sandbox": true, "eth0": false, "wlan0": false, "en0": false} {
		if got := virtualIf(name); got != want {
			t.Errorf("%s: %v", name, got)
		}
	}
	for _, a := range LANAddrs() {
		if ip := net.ParseIP(a); ip == nil || !ip.IsPrivate() || ip.To4() == nil {
			t.Errorf("%s: not a private IPv4 address", a)
		}
	}
}
