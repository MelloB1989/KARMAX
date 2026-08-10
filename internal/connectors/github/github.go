// Package github is the first native connector, chosen because it has real
// webhooks, real auth and real rate limits — so it surfaces design flaws in
// connectorkit rather than confirming a happy path.
package github

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/MelloB1989/karmax/pkg/connectorkit"
)

// api is a var so tests can point it at a stub. Never reassigned in production.
var api = "https://api.github.com"

type Connector struct{}

func New() *Connector { return &Connector{} }

func (c *Connector) Manifest() connectorkit.Manifest {
	return connectorkit.Manifest{
		ID:          "github",
		Name:        "GitHub",
		Description: "Issues, pull requests and reviews — as tools the agent can call and as events loops can trigger on.",
		Capabilities: []string{
			"http:api.github.com",
		},
		Config: []connectorkit.ConfigField{
			{Key: "token", Description: "A personal access token with repo scope", Required: true, Secret: true},
			{Key: "webhook_secret", Description: "The secret configured on the GitHub webhook, so deliveries can be verified", Secret: true},
			{Key: "default_repo", Description: "owner/name used when a tool call omits one"},
		},
	}
}

// Auth is a token rather than OAuth because a personal access token is what a
// self-hosted single operator actually has. The OAuth2 machinery exists in
// connectorkit for connectors that need it; GitHub does not, for this user.
func (c *Connector) Auth() connectorkit.AuthMethod {
	return connectorkit.AuthMethod{Kind: connectorkit.AuthAPIKey, APIKeyField: "token"}
}

func (c *Connector) Health(ctx context.Context, cr connectorkit.Credentials) error {
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

func (c *Connector) Tools() []connectorkit.Tool {
	return []connectorkit.Tool{
		{
			Name:        "github.issues",
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
			Name:        "github.comment",
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
			Name:        "github.issue",
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

// verifyDelivery checks GitHub's HMAC over the raw body.
//
// Refused when no secret is configured rather than accepted: an unverified
// webhook is an open endpoint that anything can post events into, and those
// events trigger loops.
func verifyDelivery(cr connectorkit.Credentials, headers map[string]string, body []byte) bool {
	secret := cr.Get("webhook_secret")
	if secret == "" {
		return false
	}
	sig := strings.TrimPrefix(header(headers, "X-Hub-Signature-256"), "sha256=")
	if sig == "" {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hmac.Equal([]byte(hex.EncodeToString(mac.Sum(nil))), []byte(sig))
}

// decodeDelivery turns a delivery into at most one event.
//
// Most deliveries are not interesting, and returning nothing is the normal
// case — a connector that publishes every webhook it receives makes the event
// log useless for the ones that matter.
func decodeDelivery(headers map[string]string, body []byte) ([]map[string]any, error) {
	event := header(headers, "X-GitHub-Event")
	switch event {
	case "issues", "pull_request", "issue_comment", "pull_request_review":
	default:
		return nil, nil
	}

	var d struct {
		Action     string `json:"action"`
		Repository struct {
			FullName string `json:"full_name"`
		} `json:"repository"`
		Sender struct {
			Login string `json:"login"`
		} `json:"sender"`
		Issue struct {
			Number int    `json:"number"`
			Title  string `json:"title"`
			Body   string `json:"body"`
			URL    string `json:"html_url"`
		} `json:"issue"`
		PullRequest struct {
			Number int    `json:"number"`
			Title  string `json:"title"`
			Body   string `json:"body"`
			URL    string `json:"html_url"`
			Draft  bool   `json:"draft"`
		} `json:"pull_request"`
		Comment struct {
			Body string `json:"body"`
			URL  string `json:"html_url"`
		} `json:"comment"`
	}
	if err := json.Unmarshal(body, &d); err != nil {
		return nil, fmt.Errorf("github: undecodable %s delivery: %w", event, err)
	}

	number, title, text, url := d.Issue.Number, d.Issue.Title, d.Issue.Body, d.Issue.URL
	if d.PullRequest.Number != 0 {
		number, title, text, url = d.PullRequest.Number, d.PullRequest.Title, d.PullRequest.Body, d.PullRequest.URL
	}
	if d.Comment.Body != "" {
		text, url = d.Comment.Body, d.Comment.URL
	}

	return []map[string]any{{
		"event":  event,
		"action": d.Action,
		"repo":   d.Repository.FullName,
		"number": number,
		"title":  title,
		"actor":  d.Sender.Login,
		"url":    url,
		// Everything below is written by whoever opened the issue, so it is
		// fenced by the host before it reaches a prompt.
		"body":  text,
		"draft": d.PullRequest.Draft,
	}}, nil
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
	return map[string]any{
		"repo": repo, "number": r.Number, "title": r.Title, "body": r.Body,
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
	token := cr.AccessToken
	if token == "" {
		token = cr.Get("token")
	}
	if token == "" {
		return rl, fmt.Errorf("github: no token configured")
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
