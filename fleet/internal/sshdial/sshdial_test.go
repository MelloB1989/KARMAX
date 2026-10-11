package sshdial

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestHelperRelay is the far end of the fake ssh: it connects stdio to the
// port in RELAY_ADDR, as `ssh -W` would. It is a no-op in a normal run.
func TestHelperRelay(t *testing.T) {
	addr := os.Getenv("RELAY_ADDR")
	if addr == "" {
		return
	}
	c, err := net.Dial("tcp", addr)
	if err != nil {
		os.Exit(3)
	}
	go io.Copy(c, os.Stdin)
	io.Copy(os.Stdout, c)
	os.Exit(0)
}

// fakeSSH puts an `ssh` on PATH that records its argv and, for -W, relays to
// relayAddr; for `cat`, prints a token.
func fakeSSH(t *testing.T, relayAddr string) (record string) {
	t.Helper()
	dir := t.TempDir()
	record = filepath.Join(dir, "argv")
	self, _ := os.Executable()
	script := "#!/bin/sh\necho \"$@\" >> " + record + "\n" +
		"case \"$*\" in\n" +
		"  *\" -W \"*) RELAY_ADDR=" + relayAddr + " exec " + self + " -test.run='^TestHelperRelay$' ;;\n" +
		"  *\" cat \"*) echo tok-from-host ;;\n" +
		"  *) echo 'unexpected' >&2; exit 9 ;;\n" +
		"esac\n"
	if err := os.WriteFile(filepath.Join(dir, "ssh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	return record
}

func TestDialRunsSSHWithW(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"version":"x"}`)
	}))
	defer srv.Close()
	rec := fakeSSH(t, strings.TrimPrefix(srv.URL, "http://"))

	cl := Client("pc2", "127.0.0.1:7880")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", "http://console.invalid/healthz", nil)
	resp, err := cl.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(b) != `{"version":"x"}` {
		t.Fatalf("body = %q", b)
	}
	got, _ := os.ReadFile(rec)
	if !strings.Contains(string(got), "-o BatchMode=yes pc2 -W 127.0.0.1:7880") {
		t.Fatalf("ssh was run as %q", got)
	}
	cl.CloseIdleConnections()
}

func TestDialSaysWhySSHFailed(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "ssh"), []byte("#!/bin/sh\necho 'Permission denied (publickey).' >&2\nexit 255\n"), 0o755)
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", "http://console.invalid/healthz", nil)
	_, err := Client("pc2", "127.0.0.1:7880").Do(req)
	if err == nil || !strings.Contains(err.Error(), "Permission denied") {
		t.Fatalf("err = %v, want ssh's own message", err)
	}
}

func TestCatReadsAFileOnTheHost(t *testing.T) {
	rec := fakeSSH(t, "")
	b, err := Cat(context.Background(), "pc2", "~/.config/fleet-console/client.token")
	if err != nil || strings.TrimSpace(string(b)) != "tok-from-host" {
		t.Fatalf("%q %v", b, err)
	}
	got, _ := os.ReadFile(rec)
	if !strings.Contains(string(got), "pc2 cat ~/.config/fleet-console/client.token") {
		t.Fatalf("ssh was run as %q", got)
	}
}
