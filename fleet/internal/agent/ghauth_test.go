package agent

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The image's GitHub auth (fleet.yaml github.owners), run for real:
// git-credential-fleet and the gh wrapper pick the token for the repository's
// owner, through fleet-gh-token, from the env fleetctl renders.

func ghEnv(t *testing.T, owners string) []string {
	t.Helper()
	pkg, err := filepath.Abs("../../../packaging/fleet")
	if err != nil {
		t.Fatal(err)
	}
	env := []string{
		"PATH=" + pkg + ":" + os.Getenv("PATH"),
		"HOME=" + t.TempDir(),
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1",
		"GH_TOKEN=tok-fleet",
		"GH_TOKEN_OWNER_ACME_ORG=tok-acme",
		"GH_TOKEN_OWNER_BETA=tok-beta",
	}
	if owners != "" {
		env = append(env, "FLEET_GITHUB_OWNERS="+owners)
	}
	return env
}

func TestGitCredentialFleetPicksTheOwnersToken(t *testing.T) {
	env := ghEnv(t, "acme-org beta")
	cases := map[string]string{
		"path=acme-org/web.git\n":   "tok-acme",
		"path=Acme-Org/web.git\n":   "tok-acme", // GitHub names are case-insensitive
		"path=beta/x\n":             "tok-beta",
		"path=someone/KARMAX.git\n": "tok-fleet",
		"":                          "tok-fleet", // no path: the fleet's token
	}
	for path, want := range cases {
		cmd := exec.Command("sh", "../../../packaging/fleet/git-credential-fleet", "get")
		cmd.Env = env
		cmd.Stdin = strings.NewReader("protocol=https\nhost=github.com\n" + path + "\n")
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("%q: %v", path, err)
		}
		if got := string(out); got != "username=x-access-token\npassword="+want+"\n" {
			t.Errorf("%q: got %q, want password %s", path, got, want)
		}
	}
	cmd := exec.Command("sh", "../../../packaging/fleet/git-credential-fleet", "store")
	cmd.Env = env
	cmd.Stdin = strings.NewReader("protocol=https\nhost=github.com\npassword=x\n\n")
	if out, err := cmd.Output(); err != nil || len(out) != 0 {
		t.Errorf("store: %q, %v", out, err)
	}
}

// git itself asks the helper with the path, so a push to an owner's
// repository authenticates as that owner's account.
func TestGitSendsThePathToTheHelper(t *testing.T) {
	env := ghEnv(t, "acme-org")
	cfg := filepath.Join(t.TempDir(), "gitconfig")
	b, err := os.ReadFile("../../../packaging/fleet/home/gitconfig")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfg, b, 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("git", "credential", "fill")
	cmd.Env = append(env, "GIT_CONFIG_GLOBAL="+cfg, "GIT_TERMINAL_PROMPT=0")
	cmd.Stdin = strings.NewReader("url=https://github.com/acme-org/web.git\n\n")
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "password=tok-acme\n") {
		t.Errorf("git credential fill:\n%s", out)
	}
}

func TestGhWrapperActsAsTheRepositorysOwner(t *testing.T) {
	dir := t.TempDir()
	fake := filepath.Join(dir, "realgh")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\nprintf '%s' \"$GH_TOKEN\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	repo := func(remotes ...string) string {
		d := t.TempDir()
		run := func(args ...string) {
			c := exec.Command("git", args...)
			c.Dir, c.Env = d, ghEnv(t, "")
			if out, err := c.CombinedOutput(); err != nil {
				t.Fatalf("git %v: %v\n%s", args, err, out)
			}
		}
		run("init", "-q")
		for i := 0; i+1 < len(remotes); i += 2 {
			if remotes[i] == "set-default" {
				run("config", "remote."+remotes[i+1]+".gh-resolved", "base")
				continue
			}
			run("remote", "add", remotes[i], remotes[i+1])
		}
		return d
	}
	acme := repo("origin", "git@work-gh:acme-org/web.git")
	fork := repo("origin", "https://github.com/someone/web.git", "upstream", "https://github.com/acme-org/web.git")
	chosen := repo("origin", "https://github.com/someone/web.git", "upstream", "https://github.com/acme-org/web.git", "set-default", "origin")
	plain := t.TempDir()

	cases := []struct {
		name, cwd, owners string
		extra             []string
		args              []string
		want              string
	}{
		{"-R", plain, "acme-org", nil, []string{"pr", "list", "-R", "acme-org/web"}, "tok-acme"},
		{"--repo=", plain, "acme-org", nil, []string{"pr", "list", "--repo=acme-org/web"}, "tok-acme"},
		{"-Rx", plain, "acme-org", nil, []string{"pr", "list", "-Racme-org/web"}, "tok-acme"},
		{"host/owner/repo", plain, "acme-org", nil, []string{"pr", "list", "-R", "github.com/acme-org/web"}, "tok-acme"},
		{"api repos", plain, "acme-org", nil, []string{"api", "repos/acme-org/web/pulls"}, "tok-acme"},
		{"api orgs", plain, "acme-org", nil, []string{"api", "/orgs/acme-org/packages?package_type=npm"}, "tok-acme"},
		{"url", plain, "acme-org", nil, []string{"pr", "view", "https://github.com/acme-org/web/pull/3"}, "tok-acme"},
		{"repo clone", plain, "acme-org", nil, []string{"repo", "clone", "acme-org/web", "/work/wt/web"}, "tok-acme"},
		{"GH_REPO", plain, "acme-org", []string{"GH_REPO=acme-org/web"}, []string{"pr", "list"}, "tok-acme"},
		{"cwd alias remote", acme, "acme-org", nil, []string{"pr", "create", "--fill"}, "tok-acme"},
		{"cwd placeholder", acme, "acme-org", nil, []string{"api", "repos/{owner}/{repo}/pulls"}, "tok-acme"},
		{"upstream first", fork, "acme-org", nil, []string{"pr", "list"}, "tok-acme"},
		{"set-default wins", chosen, "acme-org", nil, []string{"pr", "list"}, "tok-fleet"},
		{"-R beats cwd", acme, "acme-org", nil, []string{"pr", "list", "-R", "someone/KARMAX"}, "tok-fleet"},
		{"body URL is not the repo", acme, "acme-org", nil, []string{"pr", "create", "--body", "see https://github.com/someone/x/pull/1"}, "tok-acme"},
		{"other owner", plain, "acme-org", nil, []string{"pr", "list", "-R", "someone/KARMAX"}, "tok-fleet"},
		{"no git repo", plain, "acme-org", nil, []string{"auth", "status"}, "tok-fleet"},
		{"no owners configured", acme, "", nil, []string{"pr", "list"}, "tok-fleet"},
	}
	wrapper, err := filepath.Abs("../../../packaging/fleet/gh")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range cases {
		cmd := exec.Command("sh", append([]string{wrapper}, c.args...)...)
		cmd.Dir = c.cwd
		cmd.Env = append(append(ghEnv(t, c.owners), "FLEET_REAL_GH="+fake), c.extra...)
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if string(out) != c.want {
			t.Errorf("%s: gh %v ran with %q, want %q", c.name, c.args, out, c.want)
		}
	}
}
