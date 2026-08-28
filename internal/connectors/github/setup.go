package github

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
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
				"learns about pushes and pull requests by asking rather than being told. " +
				"NOTE: this address only starts answering once credentials are saved AND KARMAX " +
				"has been restarted — webhook routes are mounted at startup for connectors that " +
				"have credentials, so a delivery test run before that restart will 404.",
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

// AuthOptions implements connectorkit.AuthChoices.
//
// Two ways in, and they are not interchangeable. A personal access token acts
// as the HUMAN who made it, everywhere that human can reach — including private
// repositories of other orgs they happen to belong to. A GitHub App acts only
// on the repositories it was installed on, and its tokens expire by themselves.
//
// GitHub's OAuth Apps are deliberately absent: this connector has no OAuth flow,
// so offering the option would be a form that cannot work.
func (c *Connector) AuthOptions() []connectorkit.AuthOption {
	yes, no := true, false
	_ = no

	return []connectorkit.AuthOption{
		{
			ID: "app", Name: "GitHub App", Recommended: true,
			Summary: "Scoped to the repositories you install it on, with tokens that expire on " +
				"their own. The right choice for an organisation.",
			Steps: []connectorkit.SetupStep{
				{
					Title: "Create the App",
					Body: "Settings → Developer settings → GitHub Apps → New GitHub App. Under " +
						"Repository permissions give it Contents, Issues and Pull requests " +
						"(read and write) plus Metadata (read). Leave the webhook box unticked " +
						"for now — you can add that afterwards.",
					URL: "https://github.com/settings/apps/new",
				},
				{
					Title: "Generate a private key",
					Body: "On the App's page, scroll to Private keys and generate one. GitHub " +
						"downloads a .pem file once and will not show it again. Paste the whole " +
						"file below, BEGIN and END lines included.",
				},
				{
					Title: "Install it on your repositories",
					Body: "Sidebar → Install App → choose the account, then All repositories or " +
						"a selected few. An App that is created but not installed has access to " +
						"NOTHING: every call succeeds and returns an empty list, which reads like " +
						"a permissions bug rather than a missing step.",
				},
				{
					Title: "Leave installation ID blank if you are unsure",
					Body: "Save what you have and run the health check. It asks GitHub where the " +
						"App is installed and reports the number, which is otherwise only visible " +
						"in the address bar while installing.",
					Done: &yes,
				},
			},
		},
		{
			ID: "pat", Name: "Personal access token",
			Summary: "One field, works immediately. Acts as YOU everywhere you have access, and " +
				"stops working when you leave or rotate it.",
			Steps: []connectorkit.SetupStep{
				{
					Title: "Create a token",
					Body: "Fine-grained is better: pick only the repositories KARMAX should reach " +
						"and give it Contents, Issues, Pull requests and Metadata — read and " +
						"write. A classic token works too and needs the `repo` scope.",
					URL: "https://github.com/settings/tokens",
				},
				{
					Title: "Paste it below",
					Body: "GitHub shows the value once. If you lose it, generate another — it " +
						"cannot be read back.",
				},
			},
		},
	}
}

// ValidateCredentials implements connectorkit.CredentialValidator.
//
// Checks the shape of what was pasted, never the network. The failure this
// exists for is uploading the wrong file — GitHub's downloads folder also
// contains the App's public key and, often, an unrelated .pem — which
// otherwise surfaces at the first API call as an authentication error and
// sends the operator to look at permissions.
func (c *Connector) ValidateCredentials(cr connectorkit.Credentials) error {
	if !usesAppAuth(cr) {
		if tok := strings.TrimSpace(cr.Get("token")); tok != "" {
			// A 40-character hex token is the pre-2021 format GitHub revoked
			// wholesale. It will be rejected, and saying so now beats a
			// puzzling 401 later.
			if len(tok) == 40 && isHex(tok) {
				return fmt.Errorf("this looks like an old-style token — GitHub revoked that " +
					"format, so it will be rejected. Generate a new one at " +
					"github.com/settings/tokens")
			}
		}
		return nil
	}

	key := strings.TrimSpace(cr.Get("app_private_key"))
	if key == "" {
		return nil // requiredness is the form's job, not this one's
	}
	if strings.Contains(key, "PUBLIC KEY") {
		return fmt.Errorf("that is the PUBLIC key — GitHub's private key download is the file " +
			"ending .private-key.pem")
	}
	if !strings.Contains(key, "BEGIN") || !strings.Contains(key, "PRIVATE KEY") {
		return fmt.Errorf("that does not look like a PEM private key — paste or upload the whole " +
			".pem file, including the BEGIN and END lines")
	}
	if _, err := parseRSAPrivateKey(key); err != nil {
		return fmt.Errorf("the private key could not be read: %w", err)
	}

	if id := strings.TrimSpace(cr.Get("app_id")); id != "" && !isDigits(id) {
		return fmt.Errorf("the App ID is the number on the App's page (like 1234567), not its name")
	}
	return nil
}

func isHex(s string) bool {
	for _, r := range s {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')) {
			return false
		}
	}
	return true
}

func isDigits(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// CompleteCredentials implements connectorkit.CredentialCompleter.
//
// Fills in installation_id by asking GitHub where the App is installed. That
// number is only visible in the address bar during installation, and hunting
// for it afterwards is a step the software can take instead.
//
// Silent when it cannot tell: with several installations, choosing one would be
// a guess about which account was meant, and the health check lists them so a
// human can decide.
func (c *Connector) CompleteCredentials(ctx context.Context, cr connectorkit.Credentials) (map[string]string, error) {
	if !usesAppAuth(cr) || strings.TrimSpace(cr.Get("installation_id")) != "" {
		return nil, nil
	}
	if strings.TrimSpace(cr.Get("app_id")) == "" || strings.TrimSpace(cr.Get("app_private_key")) == "" {
		return nil, nil // nothing to authenticate the lookup with
	}

	ins, err := listInstallations(ctx, cr)
	if err != nil {
		return nil, err
	}
	if len(ins) != 1 {
		return nil, nil
	}
	return map[string]string{"installation_id": strconv.FormatInt(ins[0].ID, 10)}, nil
}
