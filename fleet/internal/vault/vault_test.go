package vault

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

const fakeKey = "sk-test-0123456789abcdef"

func TestASealedValueOpensUnderItsNameAndNoOther(t *testing.T) {
	v := New(filepath.Join(t.TempDir(), "state", KeyFile))
	sealed, err := v.Seal("openai", []byte(fakeKey))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(sealed, []byte(fakeKey)) {
		t.Fatal("sealed holds the value in the clear")
	}
	if got, err := v.Open("openai", sealed); err != nil || string(got) != fakeKey {
		t.Fatalf("open = %q, %v", got, err)
	}
	if _, err := v.Open("github", sealed); !errors.Is(err, ErrOpen) {
		t.Fatalf("opened under another name: %v", err)
	}
	again, _ := v.Seal("openai", []byte(fakeKey))
	if bytes.Equal(sealed, again) {
		t.Fatal("two seals of one value are the same: the nonce repeats")
	}
}

func TestTheKeyIsMadeOnceAndKeptAcrossRestarts(t *testing.T) {
	path := filepath.Join(t.TempDir(), KeyFile)
	sealed, err := New(path).Seal("openai", []byte(fakeKey))
	if err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(path)
	if err != nil || st.Mode().Perm() != 0o600 || st.Size() != 32 {
		t.Fatalf("key file: %v %v", st, err)
	}
	if got, err := New(path).Open("openai", sealed); err != nil || string(got) != fakeKey {
		t.Fatalf("a new fleetd opens = %q, %v", got, err)
	}
}

func TestAKeyFileOthersCanReadOrOfTheWrongSizeIsRefused(t *testing.T) {
	dir := t.TempDir()
	open := filepath.Join(dir, "open.key")
	_ = os.WriteFile(open, bytes.Repeat([]byte{1}, 32), 0o644)
	if _, err := New(open).Seal("openai", []byte(fakeKey)); err == nil {
		t.Fatal("sealed with a world-readable key")
	}
	short := filepath.Join(dir, "short.key")
	_ = os.WriteFile(short, []byte("short"), 0o600)
	if _, err := New(short).Seal("openai", []byte(fakeKey)); err == nil {
		t.Fatal("sealed with a short key")
	}
}
