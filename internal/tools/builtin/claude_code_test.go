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

// TestCleanupRefusesTheWholeLyznTasksDirectory is the regression test for the
// guard's backwards polarity (418cb02): it refused the shared root and
// anything above it, but permitted everything else — including "lyzn-tasks"
// itself, which resolves to <root>/lyzn-tasks, the one directory every task
// directory lives under. Deleting it deletes every task, including ones
// blocked awaiting an operator. "lyzn-tasks/" is what the recipe engine's own
// `lyzn-tasks/{{ .id }}` template renders to when `.id` is empty, so both
// spellings must refuse identically.
func TestCleanupRefusesTheWholeLyznTasksDirectory(t *testing.T) {
	for _, dir := range []string{"lyzn-tasks", "lyzn-tasks" + string(filepath.Separator)} {
		t.Run(dir, func(t *testing.T) {
			root := withStandInSharedRoot(t)
			blocked := filepath.Join(root, "lyzn-tasks", "task-awaiting-operator", "note.txt")
			if err := os.MkdirAll(filepath.Dir(blocked), 0o755); err != nil {
				t.Fatalf("could not seed a task directory: %v", err)
			}
			if err := os.WriteFile(blocked, []byte("do not delete me"), 0o644); err != nil {
				t.Fatalf("could not seed a task directory: %v", err)
			}

			tool := &ClaudeCodeTool{}
			if err := tool.Cleanup(dir, "irrelevant-session"); err == nil {
				t.Fatalf("Cleanup(%q, ...) succeeded; want a refusal", dir)
			}
			if _, err := os.Stat(blocked); err != nil {
				t.Fatalf("a blocked task's directory was removed by Cleanup(%q, ...): %v", dir, err)
			}
		})
	}
}

// TestCleanupRefusesAPathThatEscapesAboveTheSharedRoot covers a task id
// containing "..", which filepath.Join's own normalisation turns into an
// escape upward through the shared root rather than a subdirectory of it —
// reachable from the six-hour sweep because the id ultimately comes from
// LYZN.
func TestCleanupRefusesAPathThatEscapesAboveTheSharedRoot(t *testing.T) {
	root := withStandInSharedRoot(t)
	sibling := filepath.Join(filepath.Dir(root), "escaped-neighbour")
	canary := filepath.Join(sibling, "note.txt")
	if err := os.MkdirAll(sibling, 0o755); err != nil {
		t.Fatalf("could not seed the sibling directory: %v", err)
	}
	if err := os.WriteFile(canary, []byte("do not delete me"), 0o644); err != nil {
		t.Fatalf("could not seed the sibling directory: %v", err)
	}

	tool := &ClaudeCodeTool{}
	if err := tool.Cleanup(filepath.Join("..", filepath.Base(sibling)), "irrelevant-session"); err == nil {
		t.Fatal(`Cleanup("../escaped-neighbour", ...) succeeded; want a refusal`)
	}
	if _, err := os.Stat(canary); err != nil {
		t.Fatalf("a directory outside the shared root was removed: %v", err)
	}
}

// TestCleanupRefusesABareTopLevelDirectory covers hostpaths.WorkDir()
// defaulting to the operator's home directory: a working_dir of "Documents"
// resolves to <root>/Documents, one level under the shared root — exactly the
// same shape as "lyzn-tasks" itself — and is otherwise indistinguishable from
// a legitimate task-namespace directory.
func TestCleanupRefusesABareTopLevelDirectory(t *testing.T) {
	root := withStandInSharedRoot(t)
	docs := filepath.Join(root, "Documents", "taxes.pdf")
	if err := os.MkdirAll(filepath.Dir(docs), 0o755); err != nil {
		t.Fatalf("could not seed Documents: %v", err)
	}
	if err := os.WriteFile(docs, []byte("do not delete me"), 0o644); err != nil {
		t.Fatalf("could not seed Documents: %v", err)
	}

	tool := &ClaudeCodeTool{}
	if err := tool.Cleanup("Documents", "irrelevant-session"); err == nil {
		t.Fatal(`Cleanup("Documents", ...) succeeded; want a refusal`)
	}
	if _, err := os.Stat(docs); err != nil {
		t.Fatalf("a real directory of the operator's home was removed: %v", err)
	}
}

// TestCleanupRefusesAnAbsolutePathAnywhereOnDisk covers a working_dir supplied
// as an absolute path, which hostpaths.Resolve uses verbatim regardless of
// the shared root — reachable through harness.forget's own working_dir,
// which the recipe engine's docs describe as unsigned, unsandboxed data
// KARMAX interprets.
func TestCleanupRefusesAnAbsolutePathAnywhereOnDisk(t *testing.T) {
	withStandInSharedRoot(t)
	elsewhere := t.TempDir()
	canary := filepath.Join(elsewhere, "note.txt")
	if err := os.WriteFile(canary, []byte("do not delete me"), 0o644); err != nil {
		t.Fatalf("could not seed the unrelated directory: %v", err)
	}

	tool := &ClaudeCodeTool{}
	if err := tool.Cleanup(elsewhere, "irrelevant-session"); err == nil {
		t.Fatalf("Cleanup(%q, ...) succeeded; want a refusal", elsewhere)
	}
	if _, err := os.Stat(canary); err != nil {
		t.Fatalf("an absolute path outside the shared root was removed: %v", err)
	}
}
