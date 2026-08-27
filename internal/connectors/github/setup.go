package github

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/MelloB1989/karmax/pkg/connectorkit"
)

// Setting up a GitHub App has a step with no field attached: after the app is
// created, it has to be INSTALLED on the repositories it should see. Nothing in
// the config list says so, which is why people finish the form, get a valid
// app, and then find it can see nothing at all.
//
// A GitHub App that is not installed anywhere is not a broken credential — it
// is a working credential with access to zero repositories, and every error it
// produces reads like a permissions problem somewhere else.

// SetupSteps implements connectorkit.SetupGuide.
func (c *Connector) SetupSteps(cr connectorkit.Credentials, callbackURL string) []connectorkit.SetupStep {
	yes, no := true, false

	hasApp := strings.TrimSpace(cr.Get("app_id")) != "" && strings.TrimSpace(cr.Get("app_private_key")) != ""
	hasInstall := strings.TrimSpace(cr.Get("installation_id")) != ""
	hasToken := strings.TrimSpace(cr.Get("token")) != ""

	appDone, installDone := &no, &no
	if hasApp {
		appDone = &yes
	}
	if hasInstall {
		installDone = &yes
	}

	steps := []connectorkit.SetupStep{
		{
			Title: "Create a GitHub App",
			Body: "github.com/settings/apps/new (or your org's Settings → Developer settings → " +
				"GitHub Apps). Give it Repository permissions: Contents read/write, Issues " +
				"read/write, Pull requests read/write, Metadata read. A personal access token " +
				"works too and is quicker, but an App is per-repository rather than per-account " +
				"and its tokens expire on their own.",
			URL:  "https://github.com/settings/apps/new",
			Done: appDone,
		},
		{
			Title: "Paste the App ID and private key below",
			Body: "The App ID is on the app's General page. Generate a private key there too — " +
				"GitHub shows it once, as a .pem download. Paste the whole file including the " +
				"BEGIN and END lines.",
			Done: appDone,
		},
		{
			Title: "Install the App on your repositories",
			Body: installURLBody(cr) + " An App that has been created but not installed has " +
				"access to nothing: every call succeeds and returns an empty list, which reads " +
				"like a permissions bug rather than a missing step. After installing, come back " +
				"and run the health check — it reads the installation ID off GitHub so you do " +
				"not have to find it yourself.",
			URL:  installURL(cr),
			Done: installDone,
		},
	}

	if callbackURL != "" {
		steps = append(steps, connectorkit.SetupStep{
			Title: "Optional: send webhooks here",
			Body: "On the app's General page, set the webhook URL to this address and put the " +
				"same secret in webhook_secret below. Without it the agent still works, it just " +
				"learns about pushes and pull requests by asking rather than being told.",
			Value: callbackURL,
		})
	}

	if hasToken && !hasApp {
		steps = append(steps, connectorkit.SetupStep{
			Title: "Using a personal access token",
			Body: "A token is configured, so the App steps above are optional — the token is " +
				"used when no App is set up. Note it acts as YOU everywhere you have access, " +
				"which an App does not.",
			Done: &yes,
		})
	}
	return steps
}

// installURL is the app's install page, which only exists once the app does.
func installURL(cr connectorkit.Credentials) string {
	if slug := strings.TrimSpace(cr.Get("app_slug")); slug != "" {
		return "https://github.com/apps/" + slug + "/installations/new"
	}
	// Without the slug there is no direct link, so send them to the list rather
	// than to a URL that 404s.
	return "https://github.com/settings/apps"
}

func installURLBody(cr connectorkit.Credentials) string {
	if strings.TrimSpace(cr.Get("app_slug")) != "" {
		return "Open the install page and choose the repositories KARMAX should see."
	}
	return "Open the app, click Install App in the sidebar, and choose the repositories " +
		"KARMAX should see."
}

// installation is one place a GitHub App has been installed.
type installation struct {
	ID      int64 `json:"id"`
	Account struct {
		Login string `json:"login"`
	} `json:"account"`
	RepositorySelection string `json:"repository_selection"`
}

// listInstallations asks GitHub where this App is installed.
//
// Authenticated with the App JWT rather than an installation token, because the
// whole point is to find an installation id when we do not have one yet.
func listInstallations(ctx context.Context, cr connectorkit.Credentials) ([]installation, error) {
	jwt, err := appJWT(cr.Get("app_id"), cr.Get("app_private_key"), time.Now())
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, api+"/app/installations", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+jwt)
	req.Header.Set("Accept", "application/vnd.github+json")

	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("github: could not list app installations (%d)", res.StatusCode)
	}

	var out []installation
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
		return nil, err
	}
	return out, nil
}

// describeInstallations renders what listInstallations found as the sentence a
// health check should print.
func describeInstallations(ins []installation) string {
	if len(ins) == 0 {
		return "the App exists but is not installed on any repository yet — open its page on " +
			"GitHub, click Install App, and choose the repositories KARMAX should see"
	}
	var parts []string
	for _, i := range ins {
		scope := "selected repositories"
		if i.RepositorySelection == "all" {
			scope = "all repositories"
		}
		parts = append(parts, fmt.Sprintf("%s (installation_id %d, %s)", i.Account.Login, i.ID, scope))
	}
	return "installed on " + strings.Join(parts, "; ")
}
