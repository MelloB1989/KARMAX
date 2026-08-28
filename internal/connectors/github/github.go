// Package github is the first native connector, chosen because it has real
// webhooks, real auth and real rate limits — so it surfaces design flaws in
// connectorkit rather than confirming a happy path.
package github

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/MelloB1989/karmax/internal/safety"
	"github.com/MelloB1989/karmax/pkg/connectorkit"
)

// api is a var so tests can point it at a stub. Never reassigned in production.
var api = "https://api.github.com"

// Connector is one GitHub account's issues, pull requests and events.
type Connector struct {
	// account distinguishes several logins to GitHub. Empty is the primary.
	account string
}

// New builds the connector for one GitHub account.
//
// Several can be enabled at once — a work account and a personal one — each
// with its own token and its own `karmax login github --account <name>`. The
// account is part of the connector's identity rather than a field inside it, so
// the credential store keeps them apart without knowing what an account is.
func New(account string) *Connector { return &Connector{account: strings.TrimSpace(account)} }

// name qualifies a tool with the account, for every account but the primary.
//
// The primary keeps the clean name so the common single-account install reads
// as it always did, and a second account adds `github.issues@work` rather than
// renaming what was already there.
func (c *Connector) name(base string) string {
	if c.account == "" {
		return base
	}
	return base + "@" + c.account
}

func (c *Connector) Manifest() connectorkit.Manifest {
	id, name := "github", "GitHub"
	if c.account != "" {
		id, name = "github:"+c.account, "GitHub ("+c.account+")"
	}
	return connectorkit.Manifest{
		ID:          id,
		Name:        name,
		Description: "Issues, pull requests and reviews — as tools the agent can call and as events loops can trigger on.",
		Capabilities: []string{
			"http:api.github.com",
		},
		Config: []connectorkit.ConfigField{
			// Personal access token — the quick way.
			{
				Key: "token", Method: "pat", Required: true, Secret: true,
				Description: "Fine-grained or classic personal access token",
				Help: "github.com/settings/tokens → Generate new token. A fine-grained token " +
					"needs Contents, Issues, Pull requests and Metadata (read and write) on the " +
					"repositories you want KARMAX to reach. A classic token needs the `repo` scope.",
			},

			// GitHub App — the one an org should use.
			{
				Key: "app_id", Method: "app", Required: true,
				Description: "The App's numeric ID",
				Help: "On the App's page: Settings → Developer settings → GitHub Apps → your app. " +
					"It is labelled \"App ID\" near the top, and is a number like 1234567.",
			},
			{
				Key: "app_private_key", Method: "app", Required: true, Secret: true,
				Multiline: true, Accept: ".pem,.key,.txt",
				Description: "The App's PEM private key",
				Help: "Same page, under Private keys → Generate a private key. GitHub downloads a " +
					".pem file once and never shows it again. Paste the whole file, including the " +
					"BEGIN and END lines.",
			},
			{
				Key: "installation_id", Method: "app", Required: true,
				Description: "The installation this account acts as",
				Help: "You get this by INSTALLING the app (sidebar → Install App). If you have " +
					"already installed it, leave this blank and run the health check — it asks " +
					"GitHub where the app is installed and tells you the number.",
			},
			{
				Key: "app_slug", Method: "app",
				Description: "The App's URL slug",
				Help: "The last part of its public URL, github.com/apps/<slug>. Only used to link " +
					"you straight to the install page, so it is safe to leave blank.",
			},

			// Applies to either method.
			{
				Key: "webhook_secret", Secret: true,
				Description: "Shared secret for verifying webhook deliveries",
				Help: "Only needed if you point GitHub webhooks at KARMAX. Invent a long random " +
					"string, put the same one in the webhook's Secret box on GitHub, and create " +
					"the endpoint under Webhooks.",
			},
			{
				Key:         "default_repo",
				Description: "owner/name used when a tool call omits one",
				Help: "For example acme/api. Lets you say \"open an issue\" without naming the " +
					"repository every time.",
			},
		},
	}
}

// Auth is apikey-shaped either way a token is obtained: a personal access
// token is one config value, App auth is three (app_id, app_private_key,
// installation_id). connectorkit.AuthMethod cannot vary per credential and
// neither path is an OAuth redirect, so which one a call actually takes is
// decided at call time from what is configured — see tokenFor.
func (c *Connector) Auth() connectorkit.AuthMethod {
	return connectorkit.AuthMethod{Kind: connectorkit.AuthAPIKey, APIKeyField: "token"}
}

// Health distinguishes why a credential is broken rather than reporting one
// generic failure, because "no token" and "GitHub is down" want different
// operator reactions.
func (c *Connector) Health(ctx context.Context, cr connectorkit.Credentials) error {
	if usesAppAuth(cr) {
		return healthApp(ctx, cr)
	}
	return healthPAT(ctx, cr)
}

func healthPAT(ctx context.Context, cr connectorkit.Credentials) error {
	var out struct {
		Login string `json:"login"`
	}
	if _, err := call(ctx, cr, http.MethodGet, "/user", nil, &out); err != nil {
		return err
	}
	if out.Login == "" {
		return fmt.Errorf("github: the token did not identify a user")
	}
	return nil
}

// healthApp checks App auth in the order a broken setup actually fails:
// config present, key parses, then the exchange itself — which is where "not
// installed" and "GitHub unreachable" separate from each other.
func healthApp(ctx context.Context, cr connectorkit.Credentials) error {
	if err := requireAppConfig(cr); err != nil {
		return err
	}
	if _, err := parseRSAPrivateKey(cr.Get("app_private_key")); err != nil {
		return fmt.Errorf("github: app private key: %w", err)
	}

	// The step people miss: an App that has been CREATED but not INSTALLED has
	// access to no repositories. Every call then succeeds and returns an empty
	// list, which reads like a permissions bug somewhere else. Ask GitHub where
	// this App is installed and say so plainly — including the installation_id,
	// which is otherwise only visible in the URL bar during installation.
	if strings.TrimSpace(cr.Get("installation_id")) == "" {
		ins, err := listInstallations(ctx, cr)
		if err != nil {
			return fmt.Errorf("github: the App credentials look valid but installation_id is not "+
				"set, and listing the App's installations failed: %w", err)
		}
		return fmt.Errorf("github: %s — set installation_id to continue", describeInstallations(ins))
	}

	if _, err := cachedInstallationToken(ctx, cr); err != nil {
		return err
	}
	return nil
}

func (c *Connector) Tools() []connectorkit.Tool {
	return []connectorkit.Tool{
		{
			Name:        c.name("github.issues"),
			Description: "List open issues or pull requests in a repository. Use to see what is waiting.",
			Parameters: json.RawMessage(`{
				"type":"object",
				"properties":{
					"repo":{"type":"string","description":"owner/name; defaults to the configured repository"},
					"state":{"type":"string","enum":["open","closed","all"],"description":"default open"},
					"limit":{"type":"integer","description":"default 20, max 100"}
				}
			}`),
			Call: listIssues,
		},
		{
			Name:        c.name("github.comment"),
			Description: "Comment on an issue or pull request.",
			Parameters: json.RawMessage(`{
				"type":"object",
				"properties":{
					"repo":{"type":"string"},
					"number":{"type":"integer","description":"issue or PR number"},
					"body":{"type":"string"}
				},
				"required":["number","body"]
			}`),
			Call: comment,
		},
		{
			Name:        c.name("github.issue"),
			Description: "Read one issue or pull request in full, including its body and comment count.",
			Parameters: json.RawMessage(`{
				"type":"object",
				"properties":{"repo":{"type":"string"},"number":{"type":"integer"}},
				"required":["number"]
			}`),
			Call: getIssue,
		},
	}
}

// Sources mounts one webhook: GitHub delivers every subscribed event type to
// the App's single configured URL, and connectorkit ties one literal event
// kind to one mounted path — so every delivery here publishes as
// "github.event" regardless of what actually happened. decodeDelivery adds a
// "kind" field ("github.pr.opened", "github.pr.merged", ...) naming the
// specific thing for recipes to match on, since a literal per-payload bus
// event kind isn't available to a connector with this contract.
func (c *Connector) Sources() []connectorkit.EventSource {
	return []connectorkit.EventSource{{
		ID:        "webhook",
		Kind:      connectorkit.SourceWebhook,
		EventKind: "github.event",
		Path:      "/connectors/github",
		Verify:    verifyDelivery,
		Decode:    decodeDelivery,
	}}
}

func listIssues(ctx context.Context, cr connectorkit.Credentials, in map[string]any) (any, error) {
	repo, err := repoOf(cr, in)
	if err != nil {
		return nil, err
	}
	state, _ := in["state"].(string)
	if state == "" {
		state = "open"
	}
	limit := intOf(in["limit"], 20)
	if limit > 100 {
		limit = 100
	}

	var raw []struct {
		Number      int       `json:"number"`
		Title       string    `json:"title"`
		HTMLURL     string    `json:"html_url"`
		State       string    `json:"state"`
		Comments    int       `json:"comments"`
		UpdatedAt   string    `json:"updated_at"`
		PullRequest *struct{} `json:"pull_request"`
		User        struct {
			Login string `json:"login"`
		} `json:"user"`
	}
	path := fmt.Sprintf("/repos/%s/issues?state=%s&per_page=%d", repo, state, limit)
	if _, err := call(ctx, cr, http.MethodGet, path, nil, &raw); err != nil {
		return nil, err
	}

	items := make([]map[string]any, 0, len(raw))
	for _, r := range raw {
		items = append(items, map[string]any{
			"number": r.Number, "title": r.Title, "url": r.HTMLURL,
			"state": r.State, "comments": r.Comments, "author": r.User.Login,
			"updated_at": r.UpdatedAt, "is_pull_request": r.PullRequest != nil,
		})
	}
	return map[string]any{"repo": repo, "count": len(items), "items": items}, nil
}

func getIssue(ctx context.Context, cr connectorkit.Credentials, in map[string]any) (any, error) {
	repo, err := repoOf(cr, in)
	if err != nil {
		return nil, err
	}
	number := intOf(in["number"], 0)
	if number == 0 {
		return nil, fmt.Errorf("github: a number is required")
	}
	var r struct {
		Number   int    `json:"number"`
		Title    string `json:"title"`
		Body     string `json:"body"`
		State    string `json:"state"`
		HTMLURL  string `json:"html_url"`
		Comments int    `json:"comments"`
		User     struct {
			Login string `json:"login"`
		} `json:"user"`
	}
	if _, err := call(ctx, cr, http.MethodGet, fmt.Sprintf("/repos/%s/issues/%d", repo, number), nil, &r); err != nil {
		return nil, err
	}
	// Title and body were written by whoever opened this — anyone can open an
	// issue or a PR — so they reach the agent fenced, same as a webhook delivery.
	src := fmt.Sprintf("%s#%d on GitHub, opened by %s", repo, r.Number, r.User.Login)
	return map[string]any{
		"repo": repo, "number": r.Number, "title": safety.Fence(src, r.Title), "body": safety.Fence(src, r.Body),
		"state": r.State, "url": r.HTMLURL, "comments": r.Comments, "author": r.User.Login,
	}, nil
}

func comment(ctx context.Context, cr connectorkit.Credentials, in map[string]any) (any, error) {
	repo, err := repoOf(cr, in)
	if err != nil {
		return nil, err
	}
	number := intOf(in["number"], 0)
	body, _ := in["body"].(string)
	if number == 0 || strings.TrimSpace(body) == "" {
		return nil, fmt.Errorf("github: a number and a body are required")
	}
	var out struct {
		HTMLURL string `json:"html_url"`
	}
	payload, _ := json.Marshal(map[string]string{"body": body})
	if _, err := call(ctx, cr, http.MethodPost,
		fmt.Sprintf("/repos/%s/issues/%d/comments", repo, number), payload, &out); err != nil {
		return nil, err
	}
	return map[string]any{"url": out.HTMLURL, "posted": true}, nil
}

var client = &http.Client{Timeout: 30 * time.Second}

// call makes one API request and reports the rate limit alongside the result.
//
// GitHub answers 403 with a reset timestamp rather than 429 with Retry-After,
// which is exactly the provider-specific semantics §4.1 says a native connector
// exists to get right — a generic client reads that as "forbidden" and gives up
// on a credential that is fine.
func call(ctx context.Context, cr connectorkit.Credentials, method, path string, body []byte, out any) (connectorkit.RateLimit, error) {
	var rl connectorkit.RateLimit

	var reader io.Reader
	if body != nil {
		reader = strings.NewReader(string(body))
	}
	req, err := http.NewRequestWithContext(ctx, method, api+path, reader)
	if err != nil {
		return rl, err
	}
	token, err := tokenFor(ctx, cr)
	if err != nil {
		return rl, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "karmax-connector/1")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := client.Do(req)
	if err != nil {
		return rl, fmt.Errorf("github: %w", err)
	}
	defer resp.Body.Close()
	rl = readRateLimit(resp)

	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	switch {
	case resp.StatusCode == http.StatusForbidden && rl.Remaining == 0:
		return rl, fmt.Errorf("github: rate limit exhausted; it resets at %s",
			rl.ResetAt.Format(time.RFC3339))
	case resp.StatusCode == http.StatusUnauthorized:
		return rl, fmt.Errorf("github: the token was rejected — check it has not expired or been revoked")
	case resp.StatusCode == http.StatusNotFound:
		return rl, fmt.Errorf("github: %s not found, or the token cannot see it", path)
	case resp.StatusCode < 200 || resp.StatusCode > 299:
		return rl, fmt.Errorf("github: %s %s: %s", method, path, strings.TrimSpace(string(raw)))
	}
	if out != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			return rl, fmt.Errorf("github: undecodable response: %w", err)
		}
	}
	return rl, nil
}

func readRateLimit(resp *http.Response) connectorkit.RateLimit {
	var rl connectorkit.RateLimit
	if v := resp.Header.Get("X-RateLimit-Remaining"); v != "" {
		rl.Remaining, _ = strconv.Atoi(v)
	}
	if v := resp.Header.Get("X-RateLimit-Reset"); v != "" {
		if sec, err := strconv.ParseInt(v, 10, 64); err == nil {
			rl.ResetAt = time.Unix(sec, 0)
		}
	}
	// Retry-After always wins: the provider said the number, so a computed
	// backoff can only disagree with it.
	if v := resp.Header.Get("Retry-After"); v != "" {
		if sec, err := strconv.Atoi(v); err == nil {
			rl.RetryAfter = time.Duration(sec) * time.Second
		}
	}
	return rl
}

func repoOf(cr connectorkit.Credentials, in map[string]any) (string, error) {
	repo, _ := in["repo"].(string)
	if repo == "" {
		repo = cr.Get("default_repo")
	}
	repo = strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(repo, "https://github.com/"), "/"))
	if !strings.Contains(repo, "/") {
		return "", fmt.Errorf("github: need a repository as owner/name (set default_repo to avoid passing it every time)")
	}
	return repo, nil
}

func intOf(v any, def int) int {
	switch n := v.(type) {
	case int:
		return n
	case int64:
		return int(n)
	case float64:
		return int(n)
	}
	return def
}

func header(h map[string]string, key string) string {
	if v, ok := h[key]; ok {
		return v
	}
	// Header maps arrive canonicalised from net/http but not from every caller.
	for k, v := range h {
		if strings.EqualFold(k, key) {
			return v
		}
	}
	return ""
}
