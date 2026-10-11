package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadProbesReadsQuestionAndMarker(t *testing.T) {
	path := filepath.Join(t.TempDir(), "probes.txt")
	body := "# who's who\n\nwho runs delivery at the client => alice\n  what is the deal worth  =>  Contoso \n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := loadProbes(path)
	if err != nil {
		t.Fatal(err)
	}
	want := []probe{{"who runs delivery at the client", "alice"}, {"what is the deal worth", "contoso"}}
	if len(got) != len(want) {
		t.Fatalf("got %d probes, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("probe %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestLoadProbesRejectsALineWithoutAMarker(t *testing.T) {
	path := filepath.Join(t.TempDir(), "probes.txt")
	if err := os.WriteFile(path, []byte("who is alice\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadProbes(path); err == nil || !strings.Contains(err.Error(), "line 1") {
		t.Fatalf("err = %v, want one naming line 1", err)
	}
}

func TestLoadProbesExplainsAMissingFile(t *testing.T) {
	_, err := loadProbes(filepath.Join(t.TempDir(), "absent.txt"))
	if err == nil || !strings.Contains(err.Error(), "=>") {
		t.Fatalf("err = %v, want one that shows the file's format", err)
	}
}
