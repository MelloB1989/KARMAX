package github

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"
)

// The gh CLI's logins, as accounts KARMAX can read from and act as.
//
// The operator is usually logged in to several GitHub accounts on this host —
// personal, work, a bot — and `gh` already holds every one of them in the
// system keyring. KARMAX borrows a token per call with `gh auth token --user`
// rather than keeping a copy: nothing to configure, nothing to rotate, and a
// new `gh auth login` is picked up the next time accounts are listed.

// Account is one login the gh CLI holds.
type Account struct {
	Host   string // github.com, or a GitHub Enterprise host
	Login  string
	Active bool   // gh's active account for its host
	State  string // gh's own check: "success", "error", "timeout"
	Error  string // gh's explanation when State is not success
	Scopes string
	// API overrides the REST base derived from Host. Only tests set it.
	API string
}

const defaultHost = "github.com"

// Name is how KARMAX refers to the account everywhere: the login on
// github.com, login@host anywhere else, so two people with the same login on
// two servers are never taken for one.
func (a Account) Name() string {
	if a.Host == "" || strings.EqualFold(a.Host, defaultHost) {
		return a.Login
	}
	return a.Login + "@" + a.Host
}

// APIBase is the account's REST endpoint.
func (a Account) APIBase() string {
	switch {
	case a.API != "":
		return strings.TrimSuffix(a.API, "/")
	case a.Host == "" || strings.EqualFold(a.Host, defaultHost):
		return "https://api.github.com"
	default:
		return "https://" + a.Host + "/api/v3"
	}
}

// OK reports whether gh itself considers the login usable.
func (a Account) OK() bool { return a.State == "" || a.State == "success" }

// Logins lists accounts and lends their tokens. CLI is the implementation;
// tests use a fake.
type Logins interface {
	Accounts(ctx context.Context) ([]Account, error)
	Token(ctx context.Context, a Account) (string, error)
}

// CLI reaches the gh binary.
type CLI struct {
	Binary string
	// Run executes the binary and returns its stdout; stderr belongs in the
	// error. Nil runs it for real. A test replaces it.
	Run func(ctx context.Context, binary string, args ...string) ([]byte, error)
}

// Accounts lists every login gh holds, on every host, with gh's own verdict
// on each. A login gh reports as broken is returned too, so the caller can
// say so rather than have it vanish.
func (c CLI) Accounts(ctx context.Context) ([]Account, error) {
	// --json makes gh exit zero whatever state the accounts are in, and
	// without --show-token it prints no token.
	out, err := c.run(ctx, "auth", "status", "--json", "hosts")
	if err != nil {
		return nil, fmt.Errorf("gh auth status: %w", err)
	}
	return ParseAuthStatus(out)
}

// Token borrows one account's token. Never log it; the error carries only
// what gh wrote to stderr.
func (c CLI) Token(ctx context.Context, a Account) (string, error) {
	host := a.Host
	if host == "" {
		host = defaultHost
	}
	out, err := c.run(ctx, "auth", "token", "--hostname", host, "--user", a.Login)
	if err != nil {
		return "", fmt.Errorf("gh has no usable token for %s: %w", a.Name(), err)
	}
	tok := strings.TrimSpace(string(out))
	if tok == "" {
		return "", fmt.Errorf("gh returned an empty token for %s", a.Name())
	}
	return tok, nil
}

func (c CLI) run(ctx context.Context, args ...string) ([]byte, error) {
	bin := c.Binary
	if strings.TrimSpace(bin) == "" {
		bin = "gh"
	}
	if c.Run != nil {
		return c.Run(ctx, bin, args...)
	}
	return runGh(ctx, bin, args...)
}

// runGh runs gh with a deadline, keeping stdout (which may be a token) out of
// any error.
func runGh(ctx context.Context, bin string, args ...string) ([]byte, error) {
	cctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	cmd := exec.CommandContext(cctx, bin, args...)
	cmd.Env = ghEnv(os.Environ())
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if len(msg) > 300 {
			msg = msg[:300] + "…"
		}
		if msg == "" {
			return nil, err
		}
		return nil, fmt.Errorf("%v: %s", err, msg)
	}
	return stdout.Bytes(), nil
}

// ghEnv is the daemon's environment without the variables that override gh's
// own logins. With GH_TOKEN set, gh answers `auth token --user X` for every X
// with that one token, and every account would act as whoever owns it.
func ghEnv(env []string) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		k, _, _ := strings.Cut(kv, "=")
		switch k {
		case "GH_TOKEN", "GITHUB_TOKEN", "GH_ENTERPRISE_TOKEN", "GITHUB_ENTERPRISE_TOKEN", "GH_HOST":
			continue
		}
		out = append(out, kv)
	}
	return out
}

// ParseAuthStatus reads `gh auth status --json hosts`.
func ParseAuthStatus(b []byte) ([]Account, error) {
	var doc struct {
		Hosts map[string][]struct {
			State  string `json:"state"`
			Active bool   `json:"active"`
			Host   string `json:"host"`
			Login  string `json:"login"`
			Error  string `json:"error"`
			Scopes string `json:"scopes"`
		} `json:"hosts"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		return nil, fmt.Errorf("gh auth status: unreadable output: %w", err)
	}
	var out []Account
	for host, entries := range doc.Hosts {
		for _, e := range entries {
			if strings.TrimSpace(e.Login) == "" {
				continue
			}
			h := e.Host
			if h == "" {
				h = host
			}
			out = append(out, Account{Host: h, Login: e.Login, Active: e.Active,
				State: e.State, Error: e.Error, Scopes: e.Scopes})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name() < out[j].Name() })
	return out, nil
}
