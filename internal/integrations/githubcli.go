package integrations

import (
	"context"
	"fmt"
	"net/http"
	"time"

	ghconn "github.com/MelloB1989/karmax/internal/connectors/github"
	"github.com/MelloB1989/karmax/internal/hostpaths"
	"github.com/MelloB1989/karmax/internal/integration"
	sourcegh "github.com/MelloB1989/karmax/internal/sources/github"
	"github.com/MelloB1989/karmax/pkg/connectorkit"
)

// GitHub through the gh CLI: a row for gh itself, and one per login it holds.
//
// The logins are discovered, not configured, so these rows are too. Each is
// checked by reading that login's notifications — the one call the GitHub
// source depends on, which proves both that the token works and that it
// carries the scope.

// GhLogins is the gh CLI on this host.
func GhLogins() ghconn.Logins { return ghconn.CLI{Binary: hostpaths.Gh()} }

const ghLoginHelp = "https://cli.github.com/manual/gh_auth_login"

// registerGitHubLogins adds the gh row and a row per login found now. A
// login added later is registered by whoever sees it (the GitHub source).
func registerGitHubLogins(ctx context.Context, reg *integration.Registry, logins ghconn.Logins, hc *http.Client) {
	reg.Register(ghCLI(logins))
	cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	accts, err := logins.Accounts(cctx)
	if err != nil {
		return // the gh row says why
	}
	for _, a := range accts {
		RegisterGitHubLogin(reg, logins, a, hc)
	}
}

// RegisterGitHubLogin adds (or refreshes) one login's health row.
func RegisterGitHubLogin(reg *integration.Registry, logins ghconn.Logins, a ghconn.Account, hc *http.Client) {
	reg.Register(integration.CLISession(integration.Manifest{
		ID:   "gh:" + a.Name(),
		Name: "GitHub " + a.Name() + " (gh)",
		Description: "GitHub as " + a.Name() + ", through the gh CLI's login: its notifications become events, " +
			"and KARMAX comments, reviews and merges as this account when the work is this account's.",
		Kind:     integration.KindConnector,
		Account:  a.Name(),
		SetupURL: ghLoginHelp,
	}, hostpaths.Gh(), func(ctx context.Context, _ connectorkit.Credentials) error {
		tok, err := logins.Token(ctx, a)
		if err != nil {
			return fmt.Errorf("%w — run `gh auth login` for %s", err, a.Name())
		}
		return sourcegh.Probe(ctx, hc, a.APIBase(), tok)
	}))
}

// ghCLI is gh itself: installed, and holding at least one login.
func ghCLI(logins ghconn.Logins) integration.Integration {
	return integration.CLISession(integration.Manifest{
		ID:   "gh",
		Name: "GitHub CLI",
		Description: "The GitHub accounts the operator is logged in to with gh. Every one is watched for " +
			"notifications and can be acted as; `gh auth login` adds another without a restart.",
		Kind:     integration.KindConnector,
		SetupURL: ghLoginHelp,
	}, hostpaths.Gh(), func(ctx context.Context, _ connectorkit.Credentials) error {
		accts, err := logins.Accounts(ctx)
		if err != nil {
			return fmt.Errorf("%w — install gh and run `gh auth login`", err)
		}
		if len(accts) == 0 {
			return fmt.Errorf("no GitHub account is logged in — run `gh auth login`")
		}
		return nil
	})
}
