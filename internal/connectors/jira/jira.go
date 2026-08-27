// Package jira connects KARMAX to a Jira Cloud site: search and act on issues
// as tools, and turn issue and comment activity into durable events a recipe
// can await.
package jira

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/MelloB1989/karmax/pkg/connectorkit"
)

// Connector is one Jira Cloud site.
type Connector struct {
	// account distinguishes several sites, same convention as the GitHub
	// connector: empty is the primary, anything else qualifies tool names and
	// the manifest id so two sites can be enabled at once.
	account string
}

// New builds the connector for one Jira site.
func New(account string) *Connector { return &Connector{account: strings.TrimSpace(account)} }

func (c *Connector) name(base string) string {
	if c.account == "" {
		return base
	}
	return base + "@" + c.account
}

func (c *Connector) Manifest() connectorkit.Manifest {
	id, name := "jira", "Jira"
	if c.account != "" {
		id, name = "jira:"+c.account, "Jira ("+c.account+")"
	}
	return connectorkit.Manifest{
		ID:   id,
		Name: name,
		Description: "Search and act on Jira issues, and turn issue and comment activity into events a recipe " +
			"can wait on — a ticket reaching a status, a comment landing, a new report coming in.",
		// api.atlassian.com is the OAuth path's fixed host; *.atlassian.net is
		// every Jira Cloud site, which is what the API-token path talks to
		// directly since it addresses the operator's own tenant.
		Capabilities: []string{"http:api.atlassian.com", "http:*.atlassian.net"},
		Config: []connectorkit.ConfigField{
			{Key: "client_id", Description: "OAuth app client id, for `karmax login jira` (from developer.atlassian.com)"},
			{Key: "client_secret", Description: "OAuth app client secret", Secret: true},
			{Key: "email", Description: "Account email, for the API-token path (an alternative to OAuth)"},
			{Key: "api_token", Description: "API token from id.atlassian.com/manage-profile/security/api-tokens, for the API-token path", Secret: true},
			{Key: "site_url", Description: "Your Jira site, e.g. https://acme.atlassian.net — required for the API-token path, and disambiguates OAuth if the app can reach several sites"},
			{Key: "webhook_secret", Description: "Shared secret Jira must send back as X-Jira-Webhook-Secret, configured on the Automation \"Send web request\" action", Secret: true},
		},
	}
}

// Auth declares OAuth as the primary method — what `karmax login jira` runs —
// but does not gate the API-token fields behind it: they are ordinary config
// fields, so an operator who sets email, api_token and site_url in
// karmax.yaml or the environment never needs to run a login flow at all. See
// baseAndAuth, which picks whichever is actually present at call time.
func (c *Connector) Auth() connectorkit.AuthMethod {
	return connectorkit.AuthMethod{
		Kind: connectorkit.AuthOAuth2,
		OAuth2: &connectorkit.OAuth2Config{
			AuthURL:     "https://auth.atlassian.com/authorize",
			TokenURL:    "https://auth.atlassian.com/oauth/token",
			Scopes:      []string{"read:jira-work", "write:jira-work", "read:jira-user", "offline_access"},
			ClientIDKey: "client_id",
			SecretKey:   "client_secret",
		},
	}
}

// Health distinguishes what the task asks for: bad credentials, an
// unreachable site, and a site that doesn't match what was configured. myself
// is the cheapest call that proves all three at once — it needs a real
// session and it 404s if the resolved base is not actually a Jira site.
func (c *Connector) Health(ctx context.Context, cr connectorkit.Credentials) error {
	var out struct {
		AccountID   string `json:"accountId"`
		DisplayName string `json:"displayName"`
	}
	if _, err := call(ctx, cr, http.MethodGet, "/rest/api/3/myself", nil, &out); err != nil {
		return err
	}
	if out.AccountID == "" {
		return fmt.Errorf("jira: the credentials did not identify an account")
	}
	return nil
}

func (c *Connector) Tools() []connectorkit.Tool { return c.toolManifests() }

func (c *Connector) Sources() []connectorkit.EventSource {
	base := "/connectors/jira"
	if c.account != "" {
		base += "/" + c.account
	}
	return []connectorkit.EventSource{
		{
			ID:        "issue-created",
			Kind:      connectorkit.SourceWebhook,
			EventKind: EventIssueCreated,
			Path:      base + "/issue-created",
			Verify:    verifyDelivery,
			Decode:    decodeIssueCreated,
		},
		{
			ID:        "issue-updated",
			Kind:      connectorkit.SourceWebhook,
			EventKind: EventIssueUpdated,
			Path:      base + "/issue-updated",
			Verify:    verifyDelivery,
			Decode:    decodeIssueUpdated,
		},
		{
			ID:        "comment-created",
			Kind:      connectorkit.SourceWebhook,
			EventKind: EventCommentCreated,
			Path:      base + "/comment-created",
			Verify:    verifyDelivery,
			Decode:    decodeCommentCreated,
		},
		{
			// A poll rather than a fourth webhook: the customer runs KARMAX
			// inside their own VPC, where inbound webhook delivery may be
			// blocked or simply flaky, and a missed transition has to be
			// LATE rather than lost.
			ID:        reconcileID,
			Kind:      connectorkit.SourcePoll,
			EventKind: EventIssueUpdated,
			Interval:  reconcileInterval,
			Poll:      reconcilePoll,
		},
	}
}
