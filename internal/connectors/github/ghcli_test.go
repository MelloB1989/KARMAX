package github

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// What `gh auth status --json hosts` prints: one entry per login, per host,
// with gh's own verdict on each. No token is in it unless --show-token is
// passed, which KARMAX never does.
const authStatusJSON = `{"hosts":{
  "github.com":[
    {"state":"success","active":true,"host":"github.com","login":"octo-user","tokenSource":"keyring","scopes":"gist, read:org, repo","gitProtocol":"ssh"},
    {"state":"error","active":false,"host":"github.com","login":"octo-work","tokenSource":"keyring","error":"The token in keyring is invalid.","gitProtocol":"ssh"}
  ],
  "ghe.contoso.test":[
    {"state":"success","active":true,"host":"ghe.contoso.test","login":"octo-ent","tokenSource":"keyring","scopes":"repo","gitProtocol":"https"}
  ]
}}`

func TestEveryLoggedInAccountIsFoundOnEveryHost(t *testing.T) {
	accts, err := ParseAuthStatus([]byte(authStatusJSON))
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]Account{}
	for _, a := range accts {
		got[a.Name()] = a
	}
	if len(got) != 3 {
		t.Fatalf("found %v, want three accounts", got)
	}
	if a := got["octo-user"]; !a.OK() || !a.Active || a.Host != "github.com" {
		t.Errorf("octo-user = %+v, want a working, active github.com login", a)
	}
	// A broken login is still an account: it is reported, not forgotten.
	if a := got["octo-work"]; a.OK() || !strings.Contains(a.Error, "invalid") {
		t.Errorf("octo-work = %+v, want it reported as failing with gh's reason", a)
	}
	// Another host's login is named with its host, so two people called
	// octo-user on two servers are never one account.
	if a, ok := got["octo-ent@ghe.contoso.test"]; !ok || a.Login != "octo-ent" {
		t.Errorf("enterprise login = %+v, want it keyed by login@host", a)
	}
	// Sorted, so health rows and logs come out in a stable order.
	for i := 1; i < len(accts); i++ {
		if accts[i-1].Name() > accts[i].Name() {
			t.Errorf("accounts not sorted: %v before %v", accts[i-1].Name(), accts[i].Name())
		}
	}
}

func TestEachAccountTalksToItsOwnHostsAPI(t *testing.T) {
	cases := map[string]Account{
		"https://api.github.com":          {Host: "github.com", Login: "octo-user"},
		"https://ghe.contoso.test/api/v3": {Host: "ghe.contoso.test", Login: "octo-ent"},
		"http://127.0.0.1:9/stub":         {Host: "github.com", Login: "octo-user", API: "http://127.0.0.1:9/stub"},
		"https://api.github.com/":         {}, // no host is github.com
	}
	for want, a := range cases {
		if got := a.APIBase(); strings.TrimSuffix(got, "/") != strings.TrimSuffix(want, "/") {
			t.Errorf("%+v: APIBase = %q, want %q", a, got, want)
		}
	}
}

func TestDiscoveryNeverAsksGhForATokenItWouldPrint(t *testing.T) {
	var asked []string
	cli := CLI{Binary: "gh", Run: func(_ context.Context, _ string, args ...string) ([]byte, error) {
		asked = args
		return []byte(authStatusJSON), nil
	}}
	accts, err := cli.Accounts(context.Background())
	if err != nil || len(accts) != 3 {
		t.Fatalf("Accounts = %v, %v", accts, err)
	}
	joined := strings.Join(asked, " ")
	if joined != "auth status --json hosts" {
		t.Errorf("asked gh for %q, want `auth status --json hosts`", joined)
	}
	if strings.Contains(joined, "show-token") || strings.Contains(joined, " -t") {
		t.Errorf("discovery asked gh to print tokens: %q", joined)
	}
}

func TestATokenIsTheNamedAccountsOnItsHost(t *testing.T) {
	var asked []string
	cli := CLI{Binary: "gh", Run: func(_ context.Context, _ string, args ...string) ([]byte, error) {
		asked = args
		return []byte("tok-for-test\n"), nil
	}}
	tok, err := cli.Token(context.Background(), Account{Host: "ghe.contoso.test", Login: "octo-ent"})
	if err != nil || tok != "tok-for-test" {
		t.Fatalf("Token = %q, %v", tok, err)
	}
	if got := strings.Join(asked, " "); got != "auth token --hostname ghe.contoso.test --user octo-ent" {
		t.Errorf("asked gh for %q", got)
	}
}

// Whatever gh wrote to stdout may be a token; an error carries only what it
// wrote to stderr, so a failure can be logged without leaking one.
func TestAFailedTokenLookupCarriesNoStdout(t *testing.T) {
	cli := CLI{Binary: "gh", Run: func(_ context.Context, _ string, _ ...string) ([]byte, error) {
		return []byte("half-written-secret"), errors.New("no oauth token found for github.com account octo-work")
	}}
	_, err := cli.Token(context.Background(), Account{Host: "github.com", Login: "octo-work"})
	if err == nil {
		t.Fatal("a failed lookup returned no error")
	}
	if strings.Contains(err.Error(), "half-written-secret") {
		t.Errorf("the error leaked stdout: %v", err)
	}
	if !strings.Contains(err.Error(), "octo-work") {
		t.Errorf("the error does not say which account: %v", err)
	}
	if _, err := (CLI{Binary: "gh", Run: func(context.Context, string, ...string) ([]byte, error) {
		return []byte("  \n"), nil
	}}).Token(context.Background(), Account{Login: "octo-user"}); err == nil {
		t.Error("an empty token was accepted")
	}
}

// A token in the daemon's environment would make gh answer every --user with
// that one token — every account would act as whoever owns it.
func TestGhIsRunWithoutEnvironmentTokens(t *testing.T) {
	env := ghEnv([]string{"HOME=/home/octo", "GH_TOKEN=a", "GITHUB_TOKEN=b", "GH_ENTERPRISE_TOKEN=c",
		"GITHUB_ENTERPRISE_TOKEN=d", "GH_HOST=ghe.contoso.test", "PATH=/usr/bin"})
	joined := strings.Join(env, " ")
	for _, k := range []string{"GH_TOKEN=", "GITHUB_TOKEN=", "GH_ENTERPRISE_TOKEN=", "GITHUB_ENTERPRISE_TOKEN=", "GH_HOST="} {
		if strings.Contains(joined, k) {
			t.Errorf("gh would inherit %s: %v", k, env)
		}
	}
	if !strings.Contains(joined, "HOME=/home/octo") || !strings.Contains(joined, "PATH=/usr/bin") {
		t.Errorf("gh lost the environment it needs to find its keyring: %v", env)
	}
}
