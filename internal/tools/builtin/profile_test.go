package builtin

import (
	"strings"
	"testing"
)

func TestEnsureProfileHeaderAddsStamp(t *testing.T) {
	out := ensureProfileHeader("# About Nikhil\n\nFounder.")
	if !strings.HasPrefix(out, "<!-- last updated:") {
		t.Fatalf("expected stamp prefix, got: %q", out)
	}
	if !strings.Contains(out, "Founder.") {
		t.Fatalf("expected content preserved, got: %q", out)
	}
}

func TestEnsureProfileHeaderReplacesExistingStamp(t *testing.T) {
	first := ensureProfileHeader("hello")
	second := ensureProfileHeader(first) // feed an already-stamped doc back in
	if n := strings.Count(second, "<!-- last updated:"); n != 1 {
		t.Fatalf("expected exactly one stamp, got %d: %q", n, second)
	}
	if !strings.Contains(second, "hello") {
		t.Fatalf("expected content preserved, got: %q", second)
	}
}
