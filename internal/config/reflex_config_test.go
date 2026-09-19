package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// The shipped example is documentation people copy, so it has to parse.
func TestExampleReflexBlockParses(t *testing.T) {
	path, err := filepath.Abs("../../karmax.yaml.example")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Skipf("no example config: %v", err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("the example config does not load: %v", err)
	}
	if !cfg.Reflex.Enabled {
		t.Error("the example should show reflex enabled")
	}
	if cfg.Reflex.Timeout != 6*time.Second {
		t.Errorf("timeout = %v, want 6s", cfg.Reflex.Timeout)
	}
	if cfg.Reflex.Thresholds.Drop != 0.75 {
		t.Errorf("drop threshold = %v, want 0.75", cfg.Reflex.Thresholds.Drop)
	}
}
