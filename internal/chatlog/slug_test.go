package chatlog

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRemoveSessionDeletesTheTranscriptFile(t *testing.T) {
	workdir := t.TempDir()
	dir := Dir(workdir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	path := filepath.Join(dir, "sess-1.jsonl")
	if err := os.WriteFile(path, []byte("{}"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	if err := RemoveSession(workdir, "sess-1"); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("transcript still exists: %v", err)
	}
}

func TestRemoveSessionOnANonExistentFileIsNotAnError(t *testing.T) {
	if err := RemoveSession(t.TempDir(), "never-existed"); err != nil {
		t.Fatalf("removing a transcript that was never written must not error: %v", err)
	}
}
