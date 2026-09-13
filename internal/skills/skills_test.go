package skills

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSkillsMaterialiseOnFirstRun(t *testing.T) {
	dataDir := t.TempDir()

	dir, err := Materialise(dataDir)
	if err != nil {
		t.Fatalf("Materialise: %v", err)
	}
	if dir != filepath.Join(dataDir, "skills") {
		t.Fatalf("got dir %q, want %q", dir, filepath.Join(dataDir, "skills"))
	}

	for _, name := range []string{"karmax-tools", "browser-sessions"} {
		want, err := assets.ReadFile(filepath.Join(assetsRoot, name, "SKILL.md"))
		if err != nil {
			t.Fatalf("reading embedded %s: %v", name, err)
		}
		got, err := os.ReadFile(filepath.Join(dir, name, "SKILL.md"))
		if err != nil {
			t.Fatalf("reading materialised %s: %v", name, err)
		}
		if string(got) != string(want) {
			t.Errorf("%s: materialised content does not match embedded content", name)
		}
	}

	if _, err := os.Stat(filepath.Join(dir, ".shipped.json")); err != nil {
		t.Errorf("expected .shipped.json to exist: %v", err)
	}
}

func TestAnOperatorEditIsNotOverwritten(t *testing.T) {
	dataDir := t.TempDir()

	dir, err := Materialise(dataDir)
	if err != nil {
		t.Fatalf("Materialise: %v", err)
	}

	edited := filepath.Join(dir, "karmax-tools", "SKILL.md")
	const operatorContent = "the operator's own words, not what shipped"
	if err := os.WriteFile(edited, []byte(operatorContent), 0o644); err != nil {
		t.Fatalf("writing operator edit: %v", err)
	}

	if _, err := Materialise(dataDir); err != nil {
		t.Fatalf("second Materialise: %v", err)
	}

	got, err := os.ReadFile(edited)
	if err != nil {
		t.Fatalf("reading edited file: %v", err)
	}
	if string(got) != operatorContent {
		t.Errorf("restart clobbered the operator's edit: got %q", got)
	}
}

func TestAMissingSkillIsRestored(t *testing.T) {
	dataDir := t.TempDir()

	dir, err := Materialise(dataDir)
	if err != nil {
		t.Fatalf("Materialise: %v", err)
	}

	missing := filepath.Join(dir, "browser-sessions", "SKILL.md")
	if err := os.Remove(missing); err != nil {
		t.Fatalf("removing skill file: %v", err)
	}

	if _, err := Materialise(dataDir); err != nil {
		t.Fatalf("second Materialise: %v", err)
	}

	want, err := assets.ReadFile(filepath.Join(assetsRoot, "browser-sessions", "SKILL.md"))
	if err != nil {
		t.Fatalf("reading embedded content: %v", err)
	}
	got, err := os.ReadFile(missing)
	if err != nil {
		t.Fatalf("expected missing skill to be restored: %v", err)
	}
	if string(got) != string(want) {
		t.Errorf("restored content does not match embedded content")
	}
}
