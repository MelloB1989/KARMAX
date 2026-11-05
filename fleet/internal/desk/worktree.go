package desk

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"strings"

	"github.com/charmbracelet/x/ansi"

	"github.com/MelloB1989/karmax/fleet/internal/fleetapi"
)

// WorktreeWait bounds making a worktree: worktrunk runs the repository's
// pre-start hooks (a dependency install, say) before it answers.
var WorktreeWait = fleetapi.DeskWorktreeWait

// worktree is the worktree of dir's repository on branch, which a session on
// that branch starts in: worktrunk's (wt), made if there is none, and the
// branch with it when no remote has it either (from the default branch). wt
// runs in the login shell, like the sessions, so the operator's PATH and
// config are its: where worktrees go, and their hooks.
func (m *Manager) worktree(ctx context.Context, dir, branch string) (string, error) {
	if err := checkBranch(ctx, dir, branch); err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(ctx, WorktreeWait)
	defer cancel()
	args := []string{"-C", dir, "switch", branch, "--no-cd", "--format=json"}
	if !hasBranch(ctx, dir, branch) {
		args = append(args, "--create")
	}
	wt := m.WT
	if wt == "" {
		wt = "wt"
	}
	cmd := exec.CommandContext(ctx, m.Shell, append([]string{"-lc", `command -v "$0" >/dev/null 2>&1 || exit 127; exec "$0" "$@"`, wt}, args...)...)
	cmd.Dir = dir
	cmd.Env = append(m.env(), "NO_COLOR=1", "CLICOLOR=0")
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		switch {
		case ctx.Err() != nil:
			return "", invalidf("worktrunk took over %s making the worktree for %s", WorktreeWait, branch)
		case errors.As(err, &ee) && ee.ExitCode() == 127:
			return "", invalidf("worktrunk (wt) is not installed on %s: https://worktrunk.dev", m.Host)
		}
		return "", invalidf("wt: %s", wtSays(errb.String(), err))
	}
	var res struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal(lastJSON(out.Bytes()), &res); err != nil || res.Path == "" {
		return "", invalidf("wt said no worktree for %s: %s", branch, strings.TrimSpace(out.String()))
	}
	if fi, err := os.Stat(res.Path); err != nil || !fi.IsDir() {
		return "", invalidf("wt's worktree for %s, %s, is not here", branch, res.Path)
	}
	return res.Path, nil
}

// checkBranch: branch is a name git takes for a branch, and no flag.
func checkBranch(ctx context.Context, dir, branch string) error {
	if branch == "" || strings.HasPrefix(branch, "-") || strings.ContainsAny(branch, " \t\n") {
		return invalidf("branch %q: not a branch name", branch)
	}
	cmd := exec.CommandContext(ctx, "git", "check-ref-format", "--branch", branch)
	cmd.Dir = dir
	if err := cmd.Run(); err != nil {
		return invalidf("branch %q: not a branch name git takes", branch)
	}
	return nil
}

// hasBranch: dir's repository has branch, or one of its remotes does (wt
// then makes the local branch, tracking it).
func hasBranch(ctx context.Context, dir, branch string) bool {
	cmd := exec.CommandContext(ctx, "git", "for-each-ref", "--format=%(refname)", "refs/heads/"+branch, "refs/remotes/")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return false
	}
	for _, ref := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if ref == "refs/heads/"+branch {
			return true
		}
		if rest, ok := strings.CutPrefix(ref, "refs/remotes/"); ok {
			if _, b, ok := strings.Cut(rest, "/"); ok && b == branch {
				return true
			}
		}
	}
	return false
}

// lastJSON is the last line of out that is a JSON object.
func lastJSON(out []byte) []byte {
	lines := bytes.Split(bytes.TrimSpace(out), []byte("\n"))
	for i := len(lines) - 1; i >= 0; i-- {
		if l := bytes.TrimSpace(lines[i]); bytes.HasPrefix(l, []byte("{")) {
			return l
		}
	}
	return nil
}

// wtSays is what wt said went wrong, in a line or two: its ✗ lines and the
// hint after them, else its last line.
func wtSays(stderr string, err error) string {
	var keep []string
	lines := strings.Split(ansi.Strip(stderr), "\n")
	for i, l := range lines {
		l = strings.TrimSpace(l)
		if strings.HasPrefix(l, "✗") {
			keep = append(keep, strings.TrimSpace(strings.TrimPrefix(l, "✗")))
			if i+1 < len(lines) {
				if h := strings.TrimSpace(lines[i+1]); strings.HasPrefix(h, "↳") {
					keep = append(keep, strings.TrimSpace(strings.TrimPrefix(h, "↳")))
				}
			}
		}
	}
	if len(keep) == 0 {
		for i := len(lines) - 1; i >= 0; i-- {
			if l := strings.TrimSpace(lines[i]); l != "" {
				keep = append(keep, l)
				break
			}
		}
	}
	if len(keep) == 0 {
		return err.Error()
	}
	return strings.Join(keep, "; ")
}
