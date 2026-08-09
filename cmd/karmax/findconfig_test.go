package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestExplicitDataDirBeatsTheWorkingDirectory(t *testing.T) {
	// A second instance started from a directory containing someone else's
	// karmax.yaml must not adopt it. That happened: instance 2 loaded the
	// operator's config, bound their ports and attached to their WhatsApp.
	tenant := t.TempDir()
	if err := os.WriteFile(filepath.Join(tenant, "karmax.yaml"), []byte("karmax:\n  data_dir: x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cwd := t.TempDir()
	if err := os.WriteFile(filepath.Join(cwd, "karmax.yaml"), []byte("karmax:\n  data_dir: other\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	old, _ := os.Getwd()
	t.Cleanup(func() { os.Chdir(old); os.Unsetenv("KARMAX_DATA_DIR"); cfgPath = "" })
	if err := os.Chdir(cwd); err != nil {
		t.Fatal(err)
	}
	cfgPath = ""
	t.Setenv("KARMAX_DATA_DIR", tenant)

	if got := findConfig(); got != filepath.Join(tenant, "karmax.yaml") {
		t.Errorf("findConfig() = %q, want the tenant's own config", got)
	}
}

func TestWorkingDirectoryStillUsedWithoutADataDir(t *testing.T) {
	cwd := t.TempDir()
	if err := os.WriteFile(filepath.Join(cwd, "karmax.yaml"), []byte("karmax:\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	old, _ := os.Getwd()
	t.Cleanup(func() { os.Chdir(old); cfgPath = "" })
	os.Chdir(cwd)
	cfgPath = ""
	os.Unsetenv("KARMAX_DATA_DIR")

	if got := findConfig(); got != "karmax.yaml" {
		t.Errorf("findConfig() = %q, want the local karmax.yaml", got)
	}
}
