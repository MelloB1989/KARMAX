// Package x posts to the operator's X account.
//
// OAuth 1.0a rather than OAuth 2.0, which looks backwards and is not: gotwi's
// OAuth2 mode is an app-only bearer token, and app-only cannot post as a user.
// Posting needs user context, and the user-context flow that needs no browser
// round trip is 1.0a with four values the operator copies from the developer
// portal once.
package x

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/MelloB1989/karmax/internal/social"
	"github.com/MelloB1989/karmax/pkg/connectorkit"
	"github.com/michimani/gotwi"
	"github.com/michimani/gotwi/tweet/managetweet"
	"github.com/michimani/gotwi/tweet/managetweet/types"
)

// maxRunes is X's limit for a standard account.
const maxRunes = 280

// Connector is the operator's X account.
type Connector struct {
	// Forbidden is what a post may never name. Supplied by the runtime from the
	// operator's own contacts and projects, because a generic list does not know
	// who is in somebody's life.
	Forbidden func() []string
	// Limit is the rate limit and kill switch. Nil disables both, which is what
	// the login-and-health registry gets — it never posts.
	Limit *social.Limiter
}

func New(forbidden func() []string, limit *social.Limiter) *Connector {
	return &Connector{Forbidden: forbidden, Limit: limit}
}

func (c *Connector) Manifest() connectorkit.Manifest {
	return connectorkit.Manifest{
		ID:          "x",
		Name:        "X",
		Description: "Post to X as you. Every post passes the privacy guard first.",
		Capabilities: []string{"http:api.x.com", "http:api.twitter.com"},
		Config: []connectorkit.ConfigField{
			{Key: "api_key", Description: "Consumer key, from the app's Keys and tokens page", Required: true, Secret: true},
			{Key: "api_key_secret", Description: "Consumer secret", Required: true, Secret: true},
			{Key: "access_token", Description: "Access token for YOUR account (needs Read and write)", Required: true, Secret: true},
			{Key: "access_token_secret", Description: "Access token secret", Required: true, Secret: true},
		},
	}
}

func (c *Connector) Auth() connectorkit.AuthMethod {
	return connectorkit.AuthMethod{Kind: connectorkit.AuthAPIKey, APIKeyField: "access_token"}
}

func (c *Connector) Health(ctx context.Context, cr connectorkit.Credentials) error {
	client, err := clientFor(cr)
	if err != nil {
		return err
	}
	// Posting permission is what matters, and it is not implied by the tokens
	// existing: an app created with read-only access hands out tokens that work
	// for reads and fail at the first post. Checking the authenticated user is
	// the cheapest call that proves the credentials are a real user context.
	if client.AccessToken() == "" {
		return fmt.Errorf("x: no access token")
	}
	return nil
}

func (c *Connector) Tools() []connectorkit.Tool {
	return []connectorkit.Tool{{
		Name: "x.post",
		Description: "Post to X as the operator. The text is checked against the privacy rules " +
			"before it goes out, and a post that names anybody or leaks a detail is refused.",
		Parameters: json.RawMessage(`{
			"type":"object",
			"properties":{"text":{"type":"string","description":"The post, 280 characters or fewer."}},
			"required":["text"]
		}`),
		Call: c.post,
	}}
}

func (c *Connector) Sources() []connectorkit.EventSource { return nil }

func (c *Connector) post(ctx context.Context, cr connectorkit.Credentials, in map[string]any) (any, error) {
	text, _ := in["text"].(string)

	// Checked HERE, at the last point before it becomes public, rather than
	// wherever the draft was written. A guard at the call site is a guard that
	// the next caller forgets.
	guard := social.Guard{MaxRunes: maxRunes}
	if c.Forbidden != nil {
		guard.Forbidden = c.Forbidden()
	}

	return social.Publish("x", guard, c.Limit, text, func() (string, string, error) {
		client, err := clientFor(cr)
		if err != nil {
			return "", "", err
		}
		out, err := managetweet.Create(ctx, client, &types.CreateInput{Text: &text})
		if err != nil {
			return "", "", fmt.Errorf("x: the post was refused: %w", err)
		}
		id := ""
		if out != nil && out.Data.ID != nil {
			id = *out.Data.ID
		}
		return id, "https://x.com/i/status/" + id, nil
	})
}

func clientFor(cr connectorkit.Credentials) (*gotwi.Client, error) {
	for _, f := range []string{"api_key", "api_key_secret", "access_token", "access_token_secret"} {
		if strings.TrimSpace(cr.Get(f)) == "" {
			return nil, fmt.Errorf("x: %s is not configured", f)
		}
	}
	// gotwi reads the consumer pair from the environment rather than its input
	// struct, which is a wart of the library and not a decision here.
	return gotwi.NewClient(&gotwi.NewClientInput{
		AuthenticationMethod: gotwi.AuthenMethodOAuth1UserContext,
		APIKey:               cr.Get("api_key"),
		APIKeySecret:         cr.Get("api_key_secret"),
		OAuthToken:           cr.Get("access_token"),
		OAuthTokenSecret:     cr.Get("access_token_secret"),
	})
}
