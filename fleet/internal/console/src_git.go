package console

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/MelloB1989/karmax/fleet/internal/target"
)

// readRepo observes one repo: main's head, the fleet/* branches (local and
// origin's) and the commits on main in 7 days.
func readRepo(ctx context.Context, run target.Runner, path string, now time.Time) (Repo, error) {
	r := Repo{Name: filepath.Base(path), At: now, Branches: []Branch{}}
	git := func(args ...string) (string, error) {
		out, err := run.Run(ctx, append([]string{"git", "-C", path}, args...), nil)
		return strings.TrimSpace(string(out)), err
	}
	head, err := git("log", "-1", "--format=%h%x09%cI%x09%s", "main")
	if err != nil {
		return r, err
	}
	if c, ok := parseCommit(head); ok {
		r.Main = c
	}
	refs, err := git("for-each-ref", "--format=%(refname:short)%09%(objectname:short)%09%(committerdate:iso-strict)%09%(subject)",
		"refs/heads/fleet", "refs/remotes/origin/fleet")
	if err != nil {
		return r, err
	}
	byName := map[string]Branch{}
	for _, l := range strings.Split(refs, "\n") {
		f := strings.SplitN(l, "\t", 4)
		if len(f) < 4 {
			continue
		}
		name := strings.TrimPrefix(f[0], "origin/")
		at, _ := time.Parse(time.RFC3339, f[2])
		b := Branch{Name: name, SHA: f[1], At: at, Subject: f[3]}
		if parts := strings.Split(name, "/"); len(parts) >= 3 {
			b.Agent = parts[1]
		}
		if old, ok := byName[name]; !ok || b.At.After(old.At) {
			byName[name] = b
		}
	}
	for _, b := range byName {
		r.Branches = append(r.Branches, b)
	}
	sort.Slice(r.Branches, func(i, j int) bool {
		if !r.Branches[i].At.Equal(r.Branches[j].At) {
			return r.Branches[i].At.After(r.Branches[j].At)
		}
		return r.Branches[i].Name < r.Branches[j].Name
	})
	n, err := git("rev-list", "--count", "--since=7.days", "main")
	if err != nil {
		return r, err
	}
	if r.Commits7d, err = strconv.Atoi(n); err != nil {
		return r, fmt.Errorf("rev-list count %q: %w", n, err)
	}
	return r, nil
}

func parseCommit(l string) (Commit, bool) {
	f := strings.SplitN(l, "\t", 3)
	if len(f) < 3 {
		return Commit{}, false
	}
	at, _ := time.Parse(time.RFC3339, f[1])
	return Commit{SHA: f[0], At: at, Subject: f[2]}, true
}
