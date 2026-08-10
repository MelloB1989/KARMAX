package recipes

import (
	"embed"
	"os"
	"path/filepath"
	"strings"

	"go.uber.org/zap"
)

// What a fresh install starts with.
//
// The registry is where loops live, but a KARMAX that can do nothing until it
// has reached the network is a KARMAX that is broken on a plane. These four are
// embedded in the binary and written out on first run — the difference between
// "install and it works" and "install, then go find the things that make it
// useful".
//
// Embedded rather than fetched, and only these four. Every other recipe comes
// from the registry, which is what makes shipping a new one not require
// shipping a new daemon.
//
// wa-monitor also ships with KARMAX, but it is a signed WASM workflow rather
// than a recipe, so it is carried by the installer instead of here.

//go:embed builtin/*.yaml
var builtinFS embed.FS

// InstallBuiltins writes the shipped recipes into dir, and returns how many it
// wrote.
//
// Existing files are left alone. An operator who edited tech-news to change the
// digest's tone must not have that quietly reverted every time the daemon
// restarts — a default is what you get when you have not decided, not something
// that overrules you once you have.
func InstallBuiltins(dir string, log *zap.Logger) int {
	entries, err := builtinFS.ReadDir("builtin")
	if err != nil {
		return 0
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		log.Warn("could not create the recipes directory", zap.String("dir", dir), zap.Error(err))
		return 0
	}

	written := 0
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".yaml") {
			continue
		}
		path := filepath.Join(dir, e.Name())
		if _, err := os.Stat(path); err == nil {
			continue
		}
		data, err := builtinFS.ReadFile("builtin/" + e.Name())
		if err != nil {
			continue
		}
		if err := os.WriteFile(path, data, 0o644); err != nil {
			log.Warn("could not write a shipped recipe", zap.String("recipe", e.Name()), zap.Error(err))
			continue
		}
		written++
	}
	if written > 0 {
		log.Info("installed the recipes KARMAX ships with",
			zap.Int("count", written), zap.String("dir", dir))
	}
	return written
}

// BuiltinNames lists the recipes KARMAX ships with.
func BuiltinNames() []string {
	entries, err := builtinFS.ReadDir("builtin")
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if n := strings.TrimSuffix(e.Name(), ".yaml"); n != e.Name() {
			out = append(out, n)
		}
	}
	return out
}
