package builtin

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/MelloB1989/karmax/internal/hostpaths"
)

// withStandInSharedRoot points hostpaths.WorkDir() at a fresh temp directory
// for the duration of one test, and puts hostpaths back the way it found it
// afterwards. Every test below treats the returned path as if it were the
// real shared root every coding-tool call on the machine uses.
func withStandInSharedRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	t.Setenv("KARMAX_WORKDIR", root)
	hostpaths.ResetWorkDirForTest()
	t.Cleanup(hostpaths.ResetWorkDirForTest)
	return root
}

// TestCleanupRefusesEmptyWorkingDir is the regression test for a critical
// bug: Cleanup("", sessionID) resolved the empty workingDir to the SHARED
// hostpaths.WorkDir() root — the one directory every coding-tool call on the
// machine uses — and then os.RemoveAll'd it whole. Every caller today
// happens to pass a concrete per-task path, so this was latent, but a
// destructive default is one missing/forgotten argument away from wiping the
// operator's entire workspace.
//
// root stands in for the shared root. Asserting it still exists afterwards
// is what makes this test fail loudly, rather than pass by accident, if the
// guard is ever weakened or removed: without it, this test's own temp
// directory is exactly what gets deleted.
func TestCleanupRefusesEmptyWorkingDir(t *testing.T) {
	root := withStandInSharedRoot(t)

	canary := filepath.Join(root, "operators-other-work.txt")
	if err := os.WriteFile(canary, []byte("do not delete me"), 0o644); err != nil {
		t.Fatalf("could not seed the stand-in shared root: %v", err)
	}

	tool := &ClaudeCodeTool{}
	if err := tool.Cleanup("", "irrelevant-session"); err == nil {
		t.Fatal(`Cleanup("", ...) succeeded; want a refusal, not deletion of the shared working directory`)
	}

	if _, err := os.Stat(root); err != nil {
		t.Fatalf("the shared working directory stand-in was removed: %v", err)
	}
	if _, err := os.Stat(canary); err != nil {
		t.Fatalf("a file inside the shared working directory was removed: %v", err)
	}
}

// TestCleanupRefusesWhitespaceOnlyWorkingDir covers the same failure mode
// reached via a value that trims to empty without literally being "".
func TestCleanupRefusesWhitespaceOnlyWorkingDir(t *testing.T) {
	root := withStandInSharedRoot(t)

	tool := &ClaudeCodeTool{}
	if err := tool.Cleanup("   ", "irrelevant-session"); err == nil {
		t.Fatal(`Cleanup("   ", ...) succeeded; want a refusal`)
	}
	if _, err := os.Stat(root); err != nil {
		t.Fatalf("the shared working directory stand-in was removed: %v", err)
	}
}

// TestCleanupRefusesPathsThatNormaliseOntoTheSharedRoot covers "." and ".."
// — relative working_dir values that hostpaths.Resolve would otherwise land
// squarely on the shared root, or above it, exactly as an empty string does.
func TestCleanupRefusesPathsThatNormaliseOntoTheSharedRoot(t *testing.T) {
	root := withStandInSharedRoot(t)

	tool := &ClaudeCodeTool{}
	for _, dir := range []string{".", ".."} {
		if err := tool.Cleanup(dir, "irrelevant-session"); err == nil {
			t.Fatalf("Cleanup(%q, ...) succeeded; want a refusal", dir)
		}
	}
	if _, err := os.Stat(root); err != nil {
		t.Fatalf("the shared working directory stand-in was removed: %v", err)
	}
}

// TestCleanupRemovesAConcreteSubdirectory is the control: a real per-task
// path under the shared root is exactly what every current caller passes,
// and the guard must not catch that legitimate case too.
func TestCleanupRemovesAConcreteSubdirectory(t *testing.T) {
	root := withStandInSharedRoot(t)

	taskDir := filepath.Join(root, "lyzn-tasks", "task-1")
	if err := os.MkdirAll(taskDir, 0o755); err != nil {
		t.Fatalf("could not seed the task directory: %v", err)
	}

	tool := &ClaudeCodeTool{}
	if err := tool.Cleanup(filepath.Join("lyzn-tasks", "task-1"), ""); err != nil {
		t.Fatalf("Cleanup on a concrete per-task directory failed: %v", err)
	}
	if _, err := os.Stat(taskDir); !os.IsNotExist(err) {
		t.Fatalf("task directory %q still exists after Cleanup", taskDir)
	}
	if _, err := os.Stat(root); err != nil {
		t.Fatalf("the shared working directory stand-in was removed: %v", err)
	}
}
