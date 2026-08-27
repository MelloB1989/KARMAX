package jira

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/MelloB1989/karmax/pkg/connectorkit"
)

// atlassianAPI is a var so tests can point OAuth resolution at a stub. Never
// reassigned in production.
var atlassianAPI = "https://api.atlassian.com"

var httpClient = &http.Client{Timeout: 30 * time.Second}

// jiraTimeLayout is the timestamp shape Jira Cloud puts in every JSON field
// that isn't a bare date — issue.fields.updated, changelog history "created",
// webhook "timestamp"'s siblings. Not RFC3339: three-digit millis and no colon
// in the offset.
const jiraTimeLayout = "2006-01-02T15:04:05.000-0700"

func parseJiraTime(s string) (time.Time, error) {
	if strings.TrimSpace(s) == "" {
		return time.Time{}, fmt.Errorf("jira: empty timestamp")
	}
	return time.Parse(jiraTimeLayout, s)
}

// baseAndAuth picks the API root and Authorization header for whichever
// credential shape is configured.
//
// OAuth's root is api.atlassian.com/ex/jira/<cloudId>, resolved fresh per call
// rather than cached: an operator's Jira site is added to the OAuth app rarely
// enough that the extra round trip is not worth a cache that can go stale
// without anything noticing. The API-token path needs no such lookup — the
// operator already told us the site.
func baseAndAuth(ctx context.Context, cr connectorkit.Credentials) (base, auth string, err error) {
	if token := cr.AccessToken; token != "" {
		cloudID, err := resolveCloud(ctx, token, cr.Get("site_url"))
		if err != nil {
			return "", "", err
		}
		return atlassianAPI + "/ex/jira/" + cloudID, "Bearer " + token, nil
	}

	email, apiToken, site := cr.Get("email"), cr.Get("api_token"), cr.Get("site_url")
	if email == "" || apiToken == "" || site == "" {
		return "", "", fmt.Errorf("jira: not configured — either sign in with `karmax login jira` " +
			"(OAuth) or set email, api_token and site_url (an API token from id.atlassian.com/manage-profile/security/api-tokens)")
	}
	basic := base64.StdEncoding.EncodeToString([]byte(email + ":" + apiToken))
	return normalizeSite(site), "Basic " + basic, nil
}

// normalizeSite turns whatever an operator pasted into a bare origin.
func normalizeSite(raw string) string {
	s := strings.TrimSpace(raw)
	s = strings.TrimSuffix(s, "/")
	if !strings.HasPrefix(s, "http://") && !strings.HasPrefix(s, "https://") {
		s = "https://" + s
	}
	return s
}

// accessibleResource is one Jira/Confluence site an OAuth token can reach.
type accessibleResource struct {
	ID   string `json:"id"`
	URL  string `json:"url"`
	Name string `json:"name"`
}

// resolveCloud turns an OAuth access token into the cloud id its API calls
// need. hint narrows a token authorised against several sites — without one,
// several accessible sites is an error rather than a silent guess, because
// guessing wrong sends every tool call to the wrong customer's Jira.
func resolveCloud(ctx context.Context, token, hint string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		atlassianAPI+"/oauth/token/accessible-resources", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")

	resp, err := httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("jira: could not reach Atlassian to resolve the site: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode == http.StatusUnauthorized {
		return "", fmt.Errorf("jira: the OAuth token was rejected — sign in again with `karmax login jira`")
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return "", fmt.Errorf("jira: could not list accessible sites: %s", strings.TrimSpace(string(raw)))
	}

	var resources []accessibleResource
	if err := json.Unmarshal(raw, &resources); err != nil {
		return "", fmt.Errorf("jira: undecodable accessible-resources response: %w", err)
	}
	if len(resources) == 0 {
		return "", fmt.Errorf("jira: this OAuth app has not been granted access to any Jira site")
	}

	hint = strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(strings.ToLower(strings.TrimSpace(hint)),
		"https://"), "http://"), "/")
	if hint != "" {
		for _, r := range resources {
			u := strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(strings.ToLower(r.URL),
				"https://"), "http://"), "/")
			if u == hint {
				return r.ID, nil
			}
		}
		return "", fmt.Errorf("jira: site_url %q does not match any site this OAuth token can reach", hint)
	}
	if len(resources) > 1 {
		names := make([]string, len(resources))
		for i, r := range resources {
			names[i] = r.URL
		}
		return "", fmt.Errorf("jira: this token can reach several sites (%s) — set site_url to pick one",
			strings.Join(names, ", "))
	}
	return resources[0].ID, nil
}

// call makes one Jira REST API v3 request against whichever base the
// credentials resolve to.
func call(ctx context.Context, cr connectorkit.Credentials, method, path string, body []byte, out any) (connectorkit.RateLimit, error) {
	var rl connectorkit.RateLimit

	base, auth, err := baseAndAuth(ctx, cr)
	if err != nil {
		return rl, err
	}

	var reader io.Reader
	if body != nil {
		reader = strings.NewReader(string(body))
	}
	req, err := http.NewRequestWithContext(ctx, method, base+path, reader)
	if err != nil {
		return rl, err
	}
	req.Header.Set("Authorization", auth)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "karmax-connector/1")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return rl, fmt.Errorf("jira: could not reach the site: %w", err)
	}
	defer resp.Body.Close()
	rl = readRateLimit(resp)

	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	switch {
	case resp.StatusCode == http.StatusTooManyRequests:
		return rl, fmt.Errorf("jira: rate limited%s", retryHint(rl))
	case resp.StatusCode == http.StatusUnauthorized:
		return rl, fmt.Errorf("jira: the credentials were rejected — check the API token has not been revoked, " +
			"or the OAuth token has not expired")
	case resp.StatusCode == http.StatusForbidden:
		return rl, fmt.Errorf("jira: the credentials are valid but not permitted to do that " +
			"(check the account's project permissions)")
	case resp.StatusCode == http.StatusNotFound:
		return rl, fmt.Errorf("jira: %s not found, or this account cannot see it", path)
	case resp.StatusCode < 200 || resp.StatusCode > 299:
		return rl, fmt.Errorf("jira: %s %s: %s", method, path, strings.TrimSpace(string(raw)))
	}
	if out != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			return rl, fmt.Errorf("jira: undecodable response: %w", err)
		}
	}
	return rl, nil
}

func retryHint(rl connectorkit.RateLimit) string {
	if rl.RetryAfter > 0 {
		return fmt.Sprintf("; retry after %s", rl.RetryAfter)
	}
	return ""
}

// readRateLimit reads what Jira actually sends: a 429 with Retry-After, unlike
// GitHub's 403-plus-reset-timestamp shape. Nothing else is standardised across
// Jira Cloud endpoints, so nothing else is parsed.
func readRateLimit(resp *http.Response) connectorkit.RateLimit {
	var rl connectorkit.RateLimit
	if v := resp.Header.Get("Retry-After"); v != "" {
		if sec, err := strconv.Atoi(v); err == nil {
			rl.RetryAfter = time.Duration(sec) * time.Second
			rl.ResetAt = time.Now().Add(rl.RetryAfter)
		}
	}
	return rl
}

// adfNode is enough of Atlassian Document Format to read text back out of it.
// Jira v3 returns descriptions and comment bodies as ADF, and some webhook
// deliveries carry the same shape even though most still send plain strings.
type adfNode struct {
	Type    string    `json:"type"`
	Text    string    `json:"text"`
	Content []adfNode `json:"content"`
}

// adfText flattens whatever shape Jira sent — a bare string or an ADF
// document — into plain text. Malformed or empty input yields "" rather than
// an error: a body a model reads has no use for a decode failure.
func adfText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var n adfNode
	if err := json.Unmarshal(raw, &n); err != nil {
		return ""
	}
	return strings.TrimSpace(flattenADF(n))
}

func flattenADF(n adfNode) string {
	var b strings.Builder
	b.WriteString(n.Text)
	for _, c := range n.Content {
		b.WriteString(flattenADF(c))
		if c.Type == "paragraph" {
			b.WriteString("\n")
		}
	}
	return b.String()
}

// plainADF wraps plain text as the single-paragraph ADF document the v3 API
// requires for a comment body — the API refuses a bare string.
func plainADF(text string) map[string]any {
	return map[string]any{
		"type":    "doc",
		"version": 1,
		"content": []map[string]any{{
			"type":    "paragraph",
			"content": []map[string]any{{"type": "text", "text": text}},
		}},
	}
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
	for k, v := range h {
		if strings.EqualFold(k, key) {
			return v
		}
	}
	return ""
}
