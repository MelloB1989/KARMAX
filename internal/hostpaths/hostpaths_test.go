package hostpaths

import (
	"os"
	"path/filepath"
	"testing"
)

// Which Google CLI the daemon reaches for, now that there are two of them.
//
// gogcli is what the agent's tool runs; gws is still resolvable because loops
// written before the switch ask for it by name. Getting this backwards would
// send a host that has both to the one whose sessions expire.
func TestGogPrefersItsOwnOverrideAndLeavesGWSAlone(t *testing.T) {
	dir := t.TempDir()
	gog := filepath.Join(dir, "gog")
	if err := os.WriteFile(gog, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KARMAX_GOG_PATH", gog)

	if got := Gog(); got != gog {
		t.Fatalf("Gog() = %q, want the override %q", got, gog)
	}
	// The two resolvers are separate: pointing one somewhere must not move the
	// other, or a host with both installed would run whichever was asked for
	// last.
	if GWS() == gog {
		t.Fatal("GWS() resolved to the gog override")
	}
}
