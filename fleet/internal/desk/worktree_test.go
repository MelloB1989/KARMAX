package desk

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MelloB1989/karmax/fleet/internal/fleetapi"
)

// fakeWT is a stand-in for worktrunk: it logs its argv, and a switch prints
// the worktree it made where the operator's config puts it
// (~/code/worktrees/<repo>/<branch>). The branch "boom" fails as wt does.
func fakeWT(t *testing.T, home string) (bin, log string) {
	t.Helper()
	dir := t.TempDir()
	bin, log = filepath.Join(dir, "wt"), filepath.Join(dir, "wt.log")
	script := `#!/bin/sh
echo "$*" >> '` + log + `'
repo=$2 branch=$4
if [ "$branch" = boom ]; then
	echo "◎ Creating worktree..." >&2
	echo "✗ Cannot create branch boom — it collides with existing branch boo" >&2
	echo "↳ Pick a different name" >&2
	exit 1
fi
p='` + home + `'/code/worktrees/$(basename "$repo")/$(echo "$branch" | tr / -)
mkdir -p "$p"
echo "✓ Created worktree @ $p" >&2
printf '{"action":"created","branch":"%s","path":"%s"}\n' "$branch" "$p"
`
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, log
}

func gitIn(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v %s", args, err, out)
	}
}

func TestASessionOnABranchStartsInItsWorktree(t *testing.T) {
	d := newTestDesk(t)
	repo := filepath.Join(d.home, "code", "api")
	gitIn(t, repo, "init", "-q", "-b", "main")
	gitIn(t, repo, "commit", "-q", "--allow-empty", "-m", "base")
	gitIn(t, repo, "branch", "feature")
	var log string
	d.WT, log = fakeWT(t, d.home)

	// A new branch: wt makes it, and the session starts in its worktree,
	// named after it.
	s := d.create(fleetapi.DeskCreate{Dir: "~/code/api", Branch: "fleet/new-thing"})
	want := filepath.Join(d.home, "code", "worktrees", "api", "fleet-new-thing")
	if s.Dir != want || s.Name != "fleet-new-thing" {
		t.Fatalf("session %+v, want it in %s", s, want)
	}
	eventually(t, "claude in the worktree", func() bool {
		ss := d.list(10)
		return len(ss) == 1 && strings.Contains(ss[0].Text, "in "+want)
	})
	// One that exists already: no --create.
	if s := d.create(fleetapi.DeskCreate{Dir: repo, Branch: "feature", Name: "feat"}); s.Dir != filepath.Join(d.home, "code", "worktrees", "api", "feature") {
		t.Fatalf("existing branch: %+v", s)
	}
	b, _ := os.ReadFile(log)
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) != 2 || lines[0] != "-C "+repo+" switch fleet/new-thing --no-cd --format=json --create" ||
		lines[1] != "-C "+repo+" switch feature --no-cd --format=json" {
		t.Fatalf("wt ran as %q", lines)
	}

	// What wt says when it fails is the answer; no session is made.
	_, err := d.Create(context.Background(), fleetapi.DeskCreate{Dir: repo, Branch: "boom"})
	if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "Cannot create branch boom") || !strings.Contains(err.Error(), "Pick a different name") ||
		strings.Contains(err.Error(), "Creating worktree") {
		t.Fatalf("boom: %v", err)
	}
	// A name git does not take, or a flag, never reaches wt.
	for _, b := range []string{"-x", "a..b", "has space", "end."} {
		if _, err := d.Create(context.Background(), fleetapi.DeskCreate{Dir: repo, Branch: b}); !errors.Is(err, ErrInvalid) {
			t.Errorf("branch %q: %v", b, err)
		}
	}
	if b2, _ := os.ReadFile(log); strings.Count(string(b2), "\n") != 3 {
		t.Fatalf("wt ran for a bad name: %q", b2)
	}
	// Without worktrunk, it says so.
	d.WT = filepath.Join(t.TempDir(), "wt")
	if _, err := d.Create(context.Background(), fleetapi.DeskCreate{Dir: repo, Branch: "other"}); !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "worktrunk (wt) is not installed on kali") {
		t.Fatalf("no wt: %v", err)
	}
	if n := len(d.list(0)); n != 2 {
		t.Fatalf("%d sessions, want the two that started", n)
	}
}

func TestWTSaysItsErrorNotItsProgress(t *testing.T) {
	for in, want := range map[string]string{
		"◎ Running pre-start\n  npm ci\n✗ pre-start user:x failed (exit code 1)\n↳ To skip, re-run with --no-hooks\n": "pre-start user:x failed (exit code 1); To skip, re-run with --no-hooks",
		"\x1b[31m✗ No branch named x\x1b[0m\n": "No branch named x",
		"something odd\n\n":                    "something odd",
		"":                                     "exit status 1",
	} {
		if got := wtSays(in, errors.New("exit status 1")); got != want {
			t.Errorf("wtSays(%q) = %q, want %q", in, got, want)
		}
	}
}
