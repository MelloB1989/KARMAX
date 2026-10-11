package console

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/MelloB1989/karmax/fleet/internal/target"
)

func gitIn(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@example.invalid", "-c", "commit.gpgsign=false"}, args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func TestGitReportsMainBranchesAndCommits(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "demo")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	gitIn(t, dir, "init", "-q", "-b", "main")
	gitIn(t, dir, "commit", "-q", "--allow-empty", "-m", "first")
	gitIn(t, dir, "commit", "-q", "--allow-empty", "-m", "second")
	gitIn(t, dir, "branch", "fleet/agent-02/x")

	r, err := readRepo(context.Background(), target.Exec{}, dir, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if r.Name != "demo" || r.Main.Subject != "second" || r.Main.SHA == "" || r.Main.At.IsZero() {
		t.Fatalf("main = %+v", r)
	}
	if len(r.Branches) != 1 || r.Branches[0].Agent != "agent-02" || r.Branches[0].Name != "fleet/agent-02/x" {
		t.Fatalf("branches = %+v", r.Branches)
	}
	if r.Commits7d != 2 {
		t.Fatalf("commits_7d = %d", r.Commits7d)
	}
}

func TestGitErrorOnMissingRepo(t *testing.T) {
	if _, err := readRepo(context.Background(), target.Exec{}, t.TempDir(), time.Now()); err == nil {
		t.Fatal("want an error for a directory that is not a repo")
	}
}
