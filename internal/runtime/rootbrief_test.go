package runtime

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MelloB1989/karmax/internal/identity"
)

func readBrief(t *testing.T, dir string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "CLAUDE.md"))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// Every harness session inherits this file — the monitor's gateway, the memory
// extractor, a delegated coding run. It said nothing about who the operator is,
// so each session learned it from whatever its prompt and memory happened to
// say, which is how one of them took the operator for somebody else.
func TestEverySessionIsToldWhoIsWho(t *testing.T) {
	dir := t.TempDir()
	d := identity.New("Priya", []string{"15550001111", "123456789@lid"})
	if err := writeRootBrief(dir, d.Brief()); err != nil {
		t.Fatal(err)
	}
	got := readBrief(t, dir)
	for _, want := range []string{"# You are running inside KARMAX", "Who is who", "Priya", "123456789@lid", "## Reaching KARMAX"} {
		if !strings.Contains(got, want) {
			t.Errorf("brief lacks %q", want)
		}
	}
	// Ahead of everything a session reads after it.
	if strings.Index(got, "Who is who") > strings.Index(got, "## Reaching KARMAX") {
		t.Error("who-is-who comes after the rest of the brief")
	}

	// The linked account is learned after start; the file follows.
	d.SetLinked("15550002222@s.whatsapp.net")
	if err := writeRootBrief(dir, d.Brief()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(readBrief(t, dir), "15550002222") {
		t.Error("the brief was not rewritten when the linked account became known")
	}
}

// Nothing configured: the brief is exactly what it was.
func TestTheBriefIsUnchangedWithoutAnIdentity(t *testing.T) {
	dir := t.TempDir()
	if err := writeRootBrief(dir, ""); err != nil {
		t.Fatal(err)
	}
	if got := readBrief(t, dir); got != rootBrief {
		t.Errorf("brief changed with no identity configured:\n%s", got)
	}
}
