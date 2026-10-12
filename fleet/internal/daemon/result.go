package daemon

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/MelloB1989/karmax/fleet/internal/ledger"
)

// TaskAgent is a task's ledger row; ErrNoTask when the ledger has never
// heard of it.
func (f *Fleet) TaskAgent(task string) (*ledger.Task, error) {
	t, err := f.db.Task(task)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w %s", ErrNoTask, task)
	}
	return t, err
}

// TaskResult is everything a caller reads back about a task: the row, whether
// it is open, the messages filed under it and the commits on its branch.
type TaskResult struct {
	Task         ledger.Task    `json:"task"`
	State        string         `json:"state"` // open | closed
	Reports      []ledger.Relay `json:"reports"`
	Commits      []string       `json:"commits"`
	Ahead        int            `json:"ahead"`
	CommitsError string         `json:"commits_error,omitempty"`
}

// Result reads a task back. Commits come from git on fleetd's host, when the
// task has both a repo and a branch; a git failure is a note, not an error.
func (f *Fleet) Result(task string) (TaskResult, error) {
	t, err := f.TaskAgent(task)
	if err != nil {
		return TaskResult{}, err
	}
	reports, err := f.db.TaskReports(task, 0)
	if err != nil {
		return TaskResult{}, err
	}
	if reports == nil {
		reports = []ledger.Relay{}
	}
	r := TaskResult{Task: *t, State: "open", Reports: reports, Commits: []string{}}
	if !t.Closed.IsZero() {
		r.State = "closed"
	}
	if t.Repo != "" && t.Branch != "" {
		r.Commits, r.CommitsError = f.commits(t.Repo, t.Branch)
		r.Ahead = len(r.Commits)
	}
	return r, nil
}

func (f *Fleet) commits(repo, branch string) ([]string, string) {
	if strings.Contains(repo, "..") || filepath.IsAbs(repo) || strings.HasPrefix(repo, "/") {
		return []string{}, fmt.Sprintf("repo %q is not a path under the code root", repo)
	}
	if strings.HasPrefix(branch, "-") || strings.Contains(branch, "..") {
		return []string{}, fmt.Sprintf("branch %q is not a branch name", branch)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := f.git(ctx, filepath.Join(f.cfg.Code, repo), "log", "--format=%h %s", "main.."+branch)
	if err != nil {
		return []string{}, err.Error()
	}
	commits := []string{}
	for _, l := range strings.Split(out, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			commits = append(commits, l)
		}
	}
	return commits, ""
}

func runGit(ctx context.Context, dir string, args ...string) (string, error) {
	out, err := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %s: %w: %s", args[0], err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}
