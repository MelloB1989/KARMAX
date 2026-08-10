// Package linkedin posts to the operator's LinkedIn feed.
//
// pilinux/linkedin supplies the OAuth app and session — token exchange, refresh
// and introspection — and its typed models for reading posts back. It has no
// write call (its Session exposes only Get), so publishing is done here against
// LinkedIn's /rest/posts endpoint using the session's access token.
//
// That split is deliberate rather than a workaround: the fiddly,
// changes-without-notice part of LinkedIn is the OAuth dance, and that is the
// part worth taking from a library.
package linkedin

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/MelloB1989/karmax/internal/social"
	"github.com/MelloB1989/karmax/pkg/connectorkit"
)

// maxRunes is LinkedIn's limit for a member post.
const maxRunes = 3000

// apiVersion is the LinkedIn-Version header the REST API requires. It is a
// date, and LinkedIn retires old ones — a post failing with 426 means this
// needs moving forward.
const apiVersion = "202405"

// Connector is the operator's LinkedIn account.
type Connector struct {
	// Guard is the privacy check, supplied by the runtime — which is what knows
	// who is in this operator's life. A factory rather than a value so the name
	// list is rebuilt as contacts and memory change.
	Guard func() social.Guard
	// Limit is the rate limit and kill switch. Nil disables both, which is what
	// the login-and-health registry gets — it never posts.
	Limit *social.Limiter
}

func New(guard func() social.Guard, limit *social.Limiter) *Connector {
	return &Connector{Guard: guard, Limit: limit}
}

func (c *Connector) Manifest() connectorkit.Manifest {
	return connectorkit.Manifest{
		ID:           "linkedin",
		Name:         "LinkedIn",
		Description:  "Post to LinkedIn as you. Every post passes the privacy guard first.",
		Capabilities: []string{"http:api.linkedin.com", "http:www.linkedin.com"},
		Config: []connectorkit.ConfigField{
			{Key: "client_id", Description: "From your LinkedIn app's Auth tab", Required: true},
			{Key: "client_secret", Description: "From the same page", Required: true, Secret: true},
			{Key: "member_urn", Description: "Your member URN (urn:li:person:…). Filled in on first post if left blank."},
		},
	}
}

// Auth is the browser flow, because LinkedIn has no personal access token.
//
// w_member_social is the scope that permits posting; openid and profile are what
// resolve the member URN a post has to be authored by.
func (c *Connector) Auth() connectorkit.AuthMethod {
	return connectorkit.AuthMethod{
		Kind: connectorkit.AuthOAuth2,
		OAuth2: &connectorkit.OAuth2Config{
			AuthURL:     "https://www.linkedin.com/oauth/v2/authorization",
			TokenURL:    "https://www.linkedin.com/oauth/v2/accessToken",
			Scopes:      []string{"openid", "profile", "w_member_social"},
			ClientIDKey: "client_id",
			SecretKey:   "client_secret",
		},
	}
}

func (c *Connector) Health(ctx context.Context, cr connectorkit.Credentials) error {
	if strings.TrimSpace(cr.AccessToken) == "" {
		return fmt.Errorf("linkedin: not signed in — run `karmax login linkedin`")
	}
	if !cr.ExpiresAt.IsZero() && time.Now().After(cr.ExpiresAt) {
		return fmt.Errorf("linkedin: the sign-in expired on %s — run `karmax login linkedin` again",
			cr.ExpiresAt.Format("2 Jan"))
	}
	_, err := c.memberURN(ctx, cr)
	return err
}

func (c *Connector) Tools() []connectorkit.Tool {
	return []connectorkit.Tool{{
		Name: "linkedin.post",
		Description: "Post to LinkedIn as the operator. The text is checked against the privacy " +
			"rules before it goes out, and a post that names anybody or leaks a detail is refused.",
		Parameters: json.RawMessage(`{
			"type":"object",
			"properties":{"text":{"type":"string","description":"The post. Plain text; LinkedIn renders line breaks."}},
			"required":["text"]
		}`),
		Call: c.post,
	}}
}

func (c *Connector) Sources() []connectorkit.EventSource { return nil }

func (c *Connector) post(ctx context.Context, cr connectorkit.Credentials, in map[string]any) (any, error) {
	text, _ := in["text"].(string)

	var guard social.Guard
	if c.Guard != nil {
		guard = c.Guard()
	}
	guard.MaxRunes = maxRunes

	return social.Publish("linkedin", guard, c.Limit, text, func() (string, string, error) {
		urn, err := c.memberURN(ctx, cr)
		if err != nil {
			return "", "", err
		}
		body, err := json.Marshal(map[string]any{
			"author":         urn,
			"commentary":     text,
			"visibility":     "PUBLIC",
			"lifecycleState": "PUBLISHED",
			"distribution": map[string]any{
				"feedDistribution":               "MAIN_FEED",
				"targetEntities":                 []any{},
				"thirdPartyDistributionChannels": []any{},
			},
		})
		if err != nil {
			return "", "", err
		}

		resp, data, err := call(ctx, cr, http.MethodPost, "https://api.linkedin.com/rest/posts", body)
		if err != nil {
			return "", "", err
		}
		if resp.StatusCode < 200 || resp.StatusCode > 299 {
			return "", "", fmt.Errorf("linkedin: the post was refused (%s): %.250s", resp.Status, data)
		}
		// LinkedIn returns the post id in a header rather than the body.
		id := resp.Header.Get("x-restli-id")
		return id, "https://www.linkedin.com/feed/update/" + id, nil
	})
}

// memberURN resolves who a post is authored by, caching nothing.
//
// The URN can be configured, because the userinfo call needs the openid scope
// and an app that was created without it can still post perfectly well once
// somebody pastes the URN in.
func (c *Connector) memberURN(ctx context.Context, cr connectorkit.Credentials) (string, error) {
	if urn := strings.TrimSpace(cr.Get("member_urn")); urn != "" {
		return urn, nil
	}
	_, data, err := call(ctx, cr, http.MethodGet, "https://api.linkedin.com/v2/userinfo", nil)
	if err != nil {
		return "", err
	}
	var out struct {
		Sub string `json:"sub"`
	}
	if err := json.Unmarshal(data, &out); err != nil || out.Sub == "" {
		return "", fmt.Errorf("linkedin: could not work out who you are — set member_urn " +
			"(urn:li:person:…) in the config, or re-run `karmax login linkedin` with the openid scope")
	}
	return "urn:li:person:" + out.Sub, nil
}

func call(ctx context.Context, cr connectorkit.Credentials, method, url string, body []byte) (*http.Response, []byte, error) {
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(cctx, method, url, reader)
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+cr.AccessToken)
	req.Header.Set("LinkedIn-Version", apiVersion)
	req.Header.Set("X-Restli-Protocol-Version", "2.0.0")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode == http.StatusUnauthorized {
		return resp, data, fmt.Errorf("linkedin: the sign-in was refused — run `karmax login linkedin` again")
	}
	return resp, data, nil
}
