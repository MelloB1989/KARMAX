package cleanup

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/MelloB1989/karmax/fleet/internal/fleetapi"
)

// The script runs for real here: sh, find, du and git on a made-up /work,
// HOME and shared code. Everything made now is stale against a cutoff an
// hour ahead; what is in use has a file read or written after it.

func need(t *testing.T) {
	t.Helper()
	for _, c := range []string{"sh", "git", "find", "du", "stat", "touch"} {
		if _, err := exec.LookPath(c); err != nil {
			t.Skip("no " + c)
		}
	}
}

type tree struct {
	t                *testing.T
	root, work, home string
	code, repo       string // the shared code, and a checkout in it whose origin is remote
	cut              time.Time
}

func newTree(t *testing.T) *tree {
	need(t)
	root := t.TempDir()
	tr := &tree{t: t, root: root, work: filepath.Join(root, "work"), home: filepath.Join(root, "home"),
		code: filepath.Join(root, "code"), cut: time.Now().Add(time.Hour)}
	tr.repo = filepath.Join(tr.code, "repo")
	for _, d := range []string{tr.work, tr.home, tr.code} {
		tr.mkdir(d)
	}
	tr.git(root, "init", "-q", "--bare", "-b", "main", "remote.git")
	tr.git(root, "clone", "-q", filepath.Join(root, "remote.git"), tr.repo)
	tr.commit(tr.repo, "one")
	tr.git(tr.repo, "push", "-q", "origin", "main")
	return tr
}

func (tr *tree) git(dir string, args ...string) string {
	tr.t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com", "GIT_CONFIG_GLOBAL=/dev/null")
	b, err := cmd.CombinedOutput()
	if err != nil {
		tr.t.Fatalf("git %v: %v\n%s", args, err, b)
	}
	return strings.TrimSpace(string(b))
}

func (tr *tree) commit(dir, msg string) { tr.git(dir, "commit", "-q", "--allow-empty", "-m", msg) }

func (tr *tree) mkdir(p string) {
	tr.t.Helper()
	if err := os.MkdirAll(p, 0o755); err != nil {
		tr.t.Fatal(err)
	}
}

func (tr *tree) file(p, body string) {
	tr.t.Helper()
	tr.mkdir(filepath.Dir(p))
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		tr.t.Fatal(err)
	}
}

// read makes p read after the cutoff, and written before it.
func (tr *tree) read(p string) {
	tr.t.Helper()
	if err := os.Chtimes(p, tr.cut.Add(time.Hour), time.Now().Add(-48*time.Hour)); err != nil {
		tr.t.Fatal(err)
	}
}

// worktree makes a locked worktree of the shared checkout at p on branch b,
// pushed when push.
func (tr *tree) worktree(p, b string, push bool) {
	tr.t.Helper()
	tr.git(tr.repo, "worktree", "add", "-q", "--lock", p, "-b", b)
	tr.commit(p, "work on "+b)
	if push {
		tr.git(p, "push", "-q", "origin", b)
	}
}

func (tr *tree) run(dry bool, args []string) []fleetapi.CleanupItem {
	tr.t.Helper()
	cmd := exec.Command("sh", append([]string{"-c", Script, "sh"}, args...)...)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + tr.home, "FLEET_WORK=" + tr.work, "GIT_CONFIG_GLOBAL=/dev/null"}
	b, err := cmd.Output()
	if err != nil {
		var stderr string
		if ee, ok := err.(*exec.ExitError); ok {
			stderr = string(ee.Stderr)
		}
		tr.t.Fatalf("script: %v\n%s%s", err, b, stderr)
	}
	items, err := Parse(b)
	if err != nil {
		tr.t.Fatalf("parse: %v\n%s", err, b)
	}
	return items
}

func find(items []fleetapi.CleanupItem, path string) (fleetapi.CleanupItem, bool) {
	i := slices.IndexFunc(items, func(it fleetapi.CleanupItem) bool { return it.Path == path })
	if i < 0 {
		return fleetapi.CleanupItem{}, false
	}
	return items[i], true
}

func exists(p string) bool { _, err := os.Lstat(p); return err == nil }

func TestAnAgentsWorkIsDeletedOnceUnusedUnlessItHoldsWorkFoundNowhereElse(t *testing.T) {
	tr := newTree(t)
	w := tr.work
	tr.worktree(w+"/wt/merged", "fleet/agent-01/merged", true)
	tr.worktree(w+"/wt/unpushed", "fleet/agent-01/unpushed", false)
	tr.worktree(w+"/wt/dirty", "fleet/agent-01/dirty", true)
	tr.file(w+"/wt/dirty/notes.txt", "not committed")
	tr.worktree(w+"/wt/current", "fleet/agent-01/current", true)
	tr.file(w+"/wt/current/main.go", "package main")
	tr.read(w + "/wt/current/main.go")
	tr.file(w+"/scratch/out.log", "old")
	tr.git(tr.root, "clone", "-q", tr.root+"/remote.git", w+"/clone")
	tr.git(w+"/clone", "checkout", "-q", "-b", "side")
	tr.commit(w+"/clone", "on a branch never pushed")
	tr.git(w+"/clone", "checkout", "-q", "main")
	tr.file(w+"/inbox/T1/brief.pdf", "x")
	tr.file(w+"/.secrets/deploy_key", "x")
	tr.file(w+"/.fleet/current", "s1")
	tr.file(w+"/.claude/settings.local.json", "{}") // Claude Code's, for sessions in /work
	tr.file(w+"/code/repo/file", "a sandbox's code")
	// Caches: Go's build cache by entry, Go's modules by version, the rest
	// by what sits in the cache's directory.
	h := tr.home
	tr.file(h+"/.cache/go-build/ab/old-a", "x")
	tr.file(h+"/.cache/go-build/cd/used-a", "x")
	if err := os.Chtimes(h+"/.cache/go-build/cd/used-a", time.Now(), tr.cut.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	tr.file(h+"/go/pkg/mod/example.com/old@v1.0.0/go.mod", "module example.com/old")
	tr.file(h+"/go/pkg/mod/example.com/used@v1.0.0/go.mod", "module example.com/used")
	tr.read(h + "/go/pkg/mod/example.com/used@v1.0.0/go.mod")
	for _, d := range []string{"old@v1.0.0", "used@v1.0.0"} { // as Go leaves them: read-only
		if err := os.Chmod(h+"/go/pkg/mod/example.com/"+d, 0o555); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { _ = exec.Command("chmod", "-R", "u+w", h).Run() })
	tr.file(h+"/go/pkg/mod/cache/download/example.com/old/@v/v1.0.0.zip", "zip")
	tr.file(h+"/go/pkg/mod/cache/download/example.com/old/@v/v1.0.0.mod", "mod")
	tr.file(h+"/.cache/pip/http/x", "x")
	tr.file(h+"/.npm/_cacache/index", "x")
	tr.read(h + "/.npm/_cacache/index")

	items := tr.run(false, Work(tr.cut, false))

	gone := []string{w + "/wt/merged", w + "/scratch", w + "/inbox/T1", w + "/.secrets/deploy_key",
		h + "/.cache/go-build/ab/old-a", h + "/go/pkg/mod/example.com/old@v1.0.0",
		h + "/go/pkg/mod/cache/download/example.com/old/@v/v1.0.0.zip", h + "/.cache/pip"}
	for _, p := range gone {
		if exists(p) {
			t.Errorf("%s is still there", p)
		}
	}
	kept := []string{w + "/wt/unpushed", w + "/wt/dirty", w + "/wt/current", w + "/clone", w + "/inbox", w + "/.secrets",
		w + "/.fleet/current", w + "/.claude/settings.local.json", w + "/code/repo/file", h + "/.cache/go-build/cd/used-a",
		h + "/go/pkg/mod/example.com/used@v1.0.0", h + "/go/pkg/mod/cache/download/example.com/old/@v/v1.0.0.mod", h + "/.npm/_cacache"}
	for _, p := range kept {
		if !exists(p) {
			t.Errorf("%s was deleted", p)
		}
	}
	for p, why := range map[string]string{
		w + "/wt/unpushed": "1 commit no remote has",
		w + "/wt/dirty":    "uncommitted changes",
		w + "/clone":       "1 commit no remote has",
	} {
		if it, ok := find(items, p); !ok || it.Action != "kept" || !strings.Contains(it.Why, why) {
			t.Errorf("%s: %+v, want kept for %q", p, it, why)
		}
	}
	for _, p := range []string{w + "/wt/merged", w + "/scratch", w + "/inbox/T1"} {
		if it, ok := find(items, p); !ok || it.Action != "deleted" {
			t.Errorf("%s: %+v, want deleted", p, it)
		}
	}
	for _, p := range []string{w + "/wt/current", w + "/.fleet", w + "/code", h + "/.npm/_cacache"} {
		if it, ok := find(items, p); ok {
			t.Errorf("%s is in use or not the cleanup's, and was reported: %+v", p, it)
		}
	}
	if it, ok := find(items, h+"/go/pkg/mod"); !ok || it.Action != "deleted" || !strings.Contains(it.Why, "1 cache entry") {
		t.Errorf("Go's module cache: %+v, want one line for the module it deleted", it)
	}
	// The worktree's repository forgets it, lock and all.
	if l := tr.git(tr.repo, "worktree", "list"); strings.Contains(l, "/wt/merged") {
		t.Errorf("the repository still lists the deleted worktree:\n%s", l)
	}
}

func TestADryRunDeletesNothing(t *testing.T) {
	tr := newTree(t)
	tr.worktree(tr.work+"/wt/merged", "fleet/agent-01/merged", true)
	tr.file(tr.work+"/scratch/out.log", "old")
	tr.file(tr.home+"/.cache/go-build/ab/old-a", "x")

	items := tr.run(true, Work(tr.cut, true))

	for _, p := range []string{tr.work + "/wt/merged", tr.work + "/scratch", tr.home + "/.cache/go-build/ab/old-a"} {
		if !exists(p) {
			t.Errorf("a dry run deleted %s", p)
		}
		if it, ok := find(items, strings.TrimSuffix(p, "/ab/old-a")); !ok || it.Action != "would_delete" {
			t.Errorf("%s: %+v, want would_delete", p, it)
		}
	}
	if l := tr.git(tr.repo, "worktree", "list"); !strings.Contains(l, "/wt/merged") {
		t.Errorf("a dry run touched the repository:\n%s", l)
	}
}

// The check for work found nowhere else runs git, which reads git's own
// files: that is not using the item, or a dry run would keep the next pass
// from deleting anything with a checkout in it.
func TestGitReadingItsOwnFilesIsNotUse(t *testing.T) {
	tr := newTree(t)
	tr.worktree(tr.work+"/wt/merged", "fleet/agent-01/merged", true)
	tr.git(tr.work, "clone", "-q", filepath.Join(tr.root, "remote.git"), "repo")

	tr.run(true, Work(tr.cut, true))
	tr.read(tr.work + "/wt/merged/.git")
	for _, p := range []string{"HEAD", "index", "config"} {
		if exists(tr.work + "/repo/.git/" + p) {
			tr.read(tr.work + "/repo/.git/" + p)
		}
	}
	items := tr.run(false, Work(tr.cut, false))

	for _, p := range []string{tr.work + "/wt/merged", tr.work + "/repo"} {
		if it, ok := find(items, p); !ok || it.Action != "deleted" || exists(p) {
			t.Errorf("%s after git read it: %+v (there: %v), want deleted", p, it, exists(p))
		}
	}
}

// On fleetd's machine the script looks only in the directories it is given:
// each agent's under the shared code's .worktrees.
func TestTheSharedCodesWorktreesAreCleanedOnlyWhereAsked(t *testing.T) {
	tr := newTree(t)
	mine, other := tr.code+"/.worktrees/agent-05", tr.code+"/.worktrees/someone"
	tr.worktree(mine+"/merged", "fleet/agent-05/merged", true)
	tr.worktree(mine+"/unpushed", "fleet/agent-05/unpushed", false)
	tr.file(mine+"/jdk/bin/java", "a toolchain an agent unpacked")
	tr.file(other+"/notes", "not an agent's")
	if err := os.Symlink(tr.repo, mine+"/link"); err != nil {
		t.Fatal(err)
	}

	items := tr.run(false, Dirs(tr.cut, false, mine))

	for _, p := range []string{mine + "/merged", mine + "/jdk", mine + "/link"} {
		if exists(p) {
			t.Errorf("%s is still there", p)
		}
	}
	for _, p := range []string{mine + "/unpushed", other + "/notes", tr.repo + "/.git"} {
		if !exists(p) {
			t.Errorf("%s was deleted", p)
		}
	}
	if it, ok := find(items, mine+"/unpushed"); !ok || it.Action != "kept" {
		t.Errorf("unpushed: %+v", it)
	}
}

func TestParse(t *testing.T) {
	items, err := Parse([]byte("deleted\t12\t/work/scratch\tunused\nwould\t3\t/work/x\tunused\nkept\t\t/work/y\tuncommitted changes in /work/y\n\n"))
	want := []fleetapi.CleanupItem{{Action: "deleted", Bytes: 12 << 10, Path: "/work/scratch", Why: "unused"},
		{Action: "would_delete", Bytes: 3 << 10, Path: "/work/x", Why: "unused"},
		{Action: "kept", Path: "/work/y", Why: "uncommitted changes in /work/y"}}
	if err != nil || !slices.Equal(items, want) {
		t.Fatalf("%+v %v", items, err)
	}
	if _, err := Parse([]byte("deleted /work/x\n")); err == nil {
		t.Error("a line that is not the script's was taken")
	}
}

func TestNotDeletedIsTheScriptsWords(t *testing.T) {
	if strings.Count(Script, `"`+NotDeleted+`"`) != 3 {
		t.Errorf("the script says %q other than as NotDeleted", NotDeleted)
	}
}
