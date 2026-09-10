package hostpaths

import (
	"os"
	"path/filepath"
	"testing"
)

// The override wins over whatever is on PATH.
//
// A host that installed gogcli by hand, or a packaged build that ships its own
// copy, sets KARMAX_GOG_PATH — and a PATH lookup quietly beating it would run a
// different binary than the one the operator pointed at.
func TestGogPrefersItsOverride(t *testing.T) {
	dir := t.TempDir()
	gog := filepath.Join(dir, "gog")
	if err := os.WriteFile(gog, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KARMAX_GOG_PATH", gog)

	if got := Gog(); got != gog {
		t.Fatalf("Gog() = %q, want the override %q", got, gog)
	}
}

// resolve falls through env → PATH → home, and names the command when it finds
// nothing, so an exec failure says which binary is missing instead of "".
func TestResolveFallsThroughToHomeThenName(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, "go", "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	installed := filepath.Join(home, "go", "bin", "karmax-nonexistent-tool")
	if err := os.WriteFile(installed, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	got := resolve("KARMAX_NO_SUCH_OVERRIDE", "karmax-nonexistent-tool", "go/bin/karmax-nonexistent-tool")
	if got != installed {
		t.Fatalf("resolve() = %q, want the home-relative install %q", got, installed)
	}

	if got := resolve("KARMAX_NO_SUCH_OVERRIDE", "karmax-also-nonexistent"); got != "karmax-also-nonexistent" {
		t.Fatalf("resolve() = %q, want the bare command name", got)
	}
}
