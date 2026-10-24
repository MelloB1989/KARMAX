package main

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

const fakeKey = "sk-test-0123456789abcdef"

// secretFleetd answers a read of any secret with fakeKey, and counts reads.
func secretFleetd(t *testing.T) *atomic.Int32 {
	t.Helper()
	var reads atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || !strings.HasSuffix(r.URL.Path, "/read") {
			http.Error(w, `{"error":"unexpected"}`, 500)
			return
		}
		reads.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{"value": []byte(fakeKey)})
	}))
	t.Cleanup(srv.Close)
	t.Setenv("FLEETD_URL", srv.URL)
	t.Setenv("FLEET_TOKEN", "relay")
	t.Setenv("FLEET_WORK", t.TempDir())
	return &reads
}

// stdout is what f prints.
func stdout(t *testing.T, f func() error) (string, error) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdout
	os.Stdout = w
	defer func() { os.Stdout = old }()
	ferr := f()
	w.Close()
	b, _ := io.ReadAll(r)
	return string(b), ferr
}

func TestSecretExecPassesTheValueInTheEnvironmentAndPrintsNothing(t *testing.T) {
	reads := secretFleetd(t)
	out := filepath.Join(t.TempDir(), "seen")
	printed, err := stdout(t, func() error {
		return secretCmd([]string{"exec", "OPENAI_API_KEY=openai", "--", "sh", "-c", `printf %s "$OPENAI_API_KEY" > "$0"`, out})
	})
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(out); string(b) != fakeKey {
		t.Fatalf("the command saw %q", b)
	}
	if printed != "" || reads.Load() != 1 {
		t.Fatalf("printed %q, %d reads", printed, reads.Load())
	}
	// The command's exit is fleetctl's.
	err = secretCmd([]string{"exec", "K=openai", "--", "sh", "-c", "exit 3"})
	var ec exitCode
	if !errors.As(err, &ec) || ec.code != 3 {
		t.Fatalf("exit = %v", err)
	}
}

func TestSecretExecWantsNamePairsThenTheCommand(t *testing.T) {
	reads := secretFleetd(t)
	for _, args := range [][]string{
		{"exec", "--", "env"},
		{"exec", "K=openai"},
		{"exec", "K=openai", "--"},
		{"exec", "openai", "--", "env"},
		{"exec", "1K=openai", "--", "env"},
		{"exec", "K=", "--", "env"},
	} {
		if err := secretCmd(args); err == nil {
			t.Errorf("%q: want a usage error", args)
		}
	}
	if reads.Load() != 0 {
		t.Fatalf("a bad exec read %d secrets", reads.Load())
	}
}

func TestSecretGetWritesA0600FileUnderTheSecretsDirAndNowhereElse(t *testing.T) {
	reads := secretFleetd(t)
	dir := secretsDir()
	printed, err := stdout(t, func() error { return secretCmd([]string{"get", "openai", "--to", "openai.key"}) })
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "openai.key")
	st, err := os.Stat(path)
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("file: %v %v", st, err)
	}
	if b, _ := os.ReadFile(path); string(b) != fakeKey {
		t.Fatalf("file holds %q", b)
	}
	if strings.Contains(printed, fakeKey) || strings.TrimSpace(printed) != path {
		t.Fatalf("printed %q", printed)
	}
	if st, _ := os.Stat(dir); st.Mode().Perm() != 0o700 {
		t.Fatalf("dir mode %v", st.Mode().Perm())
	}

	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(dir, "out")); err != nil {
		t.Fatal(err)
	}
	n := reads.Load()
	for _, to := range []string{filepath.Join(outside, "k"), dir + "/../k", "../k", "/tmp/k", dir} {
		if err := secretCmd([]string{"get", "openai", "--to", to}); err == nil {
			t.Errorf("--to %s: written", to)
		}
	}
	if reads.Load() != n {
		t.Fatalf("a refused path still read the secret")
	}
	// A symlink in the secrets dir does not lead out of it.
	if err := secretCmd([]string{"get", "openai", "--to", filepath.Join(dir, "out", "k")}); err == nil {
		t.Fatal("written through a symlink")
	}
	if _, err := os.Stat(filepath.Join(outside, "k")); err == nil {
		t.Fatal("the value landed outside the secrets dir")
	}
}

func TestSecretGetPrintsOnlyWhenToldTo(t *testing.T) {
	secretFleetd(t)
	if err := secretCmd([]string{"get", "openai"}); err == nil {
		t.Fatal("get with neither --to nor --print: want an error")
	}
	printed, err := stdout(t, func() error { return secretCmd([]string{"get", "openai", "--print"}) })
	if err != nil || printed != fakeKey {
		t.Fatalf("printed %q, %v", printed, err)
	}
}

func TestSecretPutSendsTheFileAsItIsAndStdinWithoutItsLineEnd(t *testing.T) {
	file := filepath.Join(t.TempDir(), "id_ed25519")
	key := "-----BEGIN KEY-----\nAAAA\n-----END KEY-----\n"
	_ = os.WriteFile(file, []byte(key), 0o600)
	if v, err := secretValue(file, nil); err != nil || string(v) != key {
		t.Fatalf("file: %q %v", v, err)
	}
	r, w, _ := os.Pipe()
	go func() { _, _ = io.WriteString(w, fakeKey+"\n"); w.Close() }()
	if v, err := secretValue("", r); err != nil || string(v) != fakeKey {
		t.Fatalf("stdin: %q %v", v, err)
	}
	empty, closed, _ := os.Pipe()
	closed.Close()
	if _, err := secretValue("", empty); err == nil {
		t.Fatal("an empty stdin: want an error")
	}

	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()
	t.Setenv("FLEETD_URL", srv.URL)
	t.Setenv("FLEET_TOKEN", "full")
	if _, err := stdout(t, func() error { return secretCmd([]string{"put", "deploy-key", "--file", file}) }); err != nil {
		t.Fatal(err)
	}
	if got["name"] != "deploy-key" || got["value"] != base64.StdEncoding.EncodeToString([]byte(key)) {
		t.Fatalf("sent %v", got)
	}
}
