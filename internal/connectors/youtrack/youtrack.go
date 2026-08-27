// Package youtrack connects KARMAX to a YouTrack instance.
//
// The tool surface is deliberately small. YouTrack's REST API is enormous and
// most of it is administration an agent has no business touching; what an
// assistant actually needs is to find issues, read one, file one, comment, and
// move it along. Every tool is context in the prompt and one more choice the
// model has to get right, so six good ones beat sixty faithful ones.
//
// This replaces reaching YouTrack through the `yt` CLI over shell.exec, which
// worked but meant the agent needed a shell to file a ticket — a very large
// permission for a very small job.
package youtrack

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/MelloB1989/karmax/pkg/connectorkit"
)

var httpClient = &http.Client{Timeout: 30 * time.Second}

// Connector is one YouTrack instance.
//
// Multi-account for the same reason Jira is: an org with two YouTrack
// instances has two of everything, and the agent has to act as the right one
// rather than whichever token happened to load first.
type Connector struct {
	account string
}

// New builds the connector for one YouTrack instance.
func New(account string) *Connector { return &Connector{account: strings.TrimSpace(account)} }

// name qualifies a tool with its account, so github.issues@work reads as the
// identity it acts under.
func (c *Connector) name(base string) string {
	if c.account == "" {
		return base
	}
	return base + "@" + c.account
}

func (c *Connector) Manifest() connectorkit.Manifest {
	id, name := "youtrack", "YouTrack"
	if c.account != "" {
		id, name = "youtrack:"+c.account, "YouTrack ("+c.account+")"
	}
	return connectorkit.Manifest{
		ID:   id,
		Name: name,
		Description: "Search, read, file and update YouTrack issues, so tickets can be raised and " +
			"moved from a conversation instead of a browser tab.",
		Capabilities: []string{"http:youtrack.cloud"},
		Config: []connectorkit.ConfigField{
			{
				Key:         "base_url",
				Description: "Your instance, e.g. https://acme.youtrack.cloud (no trailing /api)",
				Required:    true,
			},
			{
				Key:         "token",
				Description: "A permanent token (perm:…) from Profile → Account Security → Authentication",
				Required:    true,
				Secret:      true,
			},
			{
				Key:         "default_project",
				Description: "Short name of the project new issues go to when none is given, e.g. LAMB",
			},
		},
	}
}

// Auth is a permanent token rather than OAuth.
//
// YouTrack's OAuth exists for apps acting on behalf of many users. KARMAX is
// installed by the operator into their own instance, where a permanent token is
// what YouTrack hands you and there is nobody to authorise on behalf of.
func (c *Connector) Auth() connectorkit.AuthMethod {
	return connectorkit.AuthMethod{Kind: connectorkit.AuthAPIKey, APIKeyField: "token"}
}

// base returns the API root, tolerating the two URLs people paste.
//
// Half of setup failures here are a base_url that already ends in /api, or one
// with a trailing slash; both are unambiguous, so they are accepted rather than
// bounced back as a config error the operator has to think about.
func base(cr connectorkit.Credentials) (string, error) {
	raw := strings.TrimSpace(cr.Get("base_url"))
	if raw == "" {
		return "", fmt.Errorf("youtrack: not configured — set base_url to your instance URL")
	}
	raw = strings.TrimRight(raw, "/")
	raw = strings.TrimSuffix(raw, "/api")
	if !strings.HasPrefix(raw, "http://") && !strings.HasPrefix(raw, "https://") {
		raw = "https://" + raw
	}
	return raw + "/api", nil
}

func token(cr connectorkit.Credentials) (string, error) {
	t := strings.TrimSpace(cr.Get("token"))
	if t == "" {
		t = strings.TrimSpace(cr.AccessToken)
	}
	if t == "" {
		return "", fmt.Errorf("youtrack: no token configured")
	}
	return t, nil
}

// call makes one authenticated request and decodes the result.
func call(ctx context.Context, cr connectorkit.Credentials, method, path string, body any, out any) error {
	root, err := base(cr)
	if err != nil {
		return err
	}
	tok, err := token(cr)
	if err != nil {
		return err
	}

	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(raw)
	}

	req, err := http.NewRequestWithContext(ctx, method, root+path, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	res, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()

	raw, _ := io.ReadAll(io.LimitReader(res.Body, 4<<20))
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return apiError(res.StatusCode, raw)
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(raw, out)
}

// apiError turns a failure into something an operator can act on.
//
// YouTrack answers a bad token with 401 and a body that does not say so, and
// answers an unknown field with a 400 whose useful part is buried — so the
// status is translated where the meaning is unambiguous and the body is
// included where it is not.
func apiError(status int, body []byte) error {
	var e struct {
		Error            string `json:"error"`
		ErrorDescription string `json:"error_description"`
	}
	_ = json.Unmarshal(body, &e)
	detail := strings.TrimSpace(e.ErrorDescription)
	if detail == "" {
		detail = strings.TrimSpace(e.Error)
	}
	if detail == "" {
		detail = strings.TrimSpace(string(body))
	}
	if len(detail) > 400 {
		detail = detail[:400] + "…"
	}

	switch status {
	case http.StatusUnauthorized:
		return fmt.Errorf("youtrack: the token was rejected (401) — it may be expired or revoked: %s", detail)
	case http.StatusForbidden:
		return fmt.Errorf("youtrack: the token lacks permission for this (403): %s", detail)
	case http.StatusNotFound:
		return fmt.Errorf("youtrack: not found (404) — check the issue id or project: %s", detail)
	default:
		return fmt.Errorf("youtrack: request failed (%d): %s", status, detail)
	}
}

func (c *Connector) Health(ctx context.Context, cr connectorkit.Credentials) error {
	cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()

	var me struct {
		Login string `json:"login"`
		Name  string `json:"name"`
	}
	// Asking who we are is the cheapest call that proves the token works and
	// tells the operator which account it belongs to — the failure people hit
	// is a valid token for the wrong user.
	if err := call(cctx, cr, http.MethodGet, "/users/me?fields=login,name", nil, &me); err != nil {
		return err
	}
	if me.Login == "" {
		return fmt.Errorf("youtrack: the instance answered but named no user — is base_url right?")
	}
	return nil
}

func (c *Connector) Sources() []connectorkit.EventSource { return nil }
