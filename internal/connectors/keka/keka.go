// Package keka connects KARMAX to Keka, the HR system.
//
// The tool surface is read-mostly on purpose. An agent that can look up who
// somebody's manager is, or who is on leave on Thursday, answers most of what
// people actually ask an HR assistant. An agent that can APPROVE leave is a
// different risk class, and that belongs behind a proposal an operator reads —
// not behind a tool the model can reach for on its own.
package keka

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/MelloB1989/karmax/pkg/connectorkit"
)

// loginHost is a var so tests can point the token exchange at a stub.
var loginHost = "https://login.keka.com"

var httpClient = &http.Client{Timeout: 30 * time.Second}

// Connector is one Keka tenant.
type Connector struct{}

func New() *Connector { return &Connector{} }

func (c *Connector) Manifest() connectorkit.Manifest {
	return connectorkit.Manifest{
		ID:   "keka",
		Name: "Keka",
		Description: "The org chart, leave and attendance as the agent can read them — so " +
			"\"who reports to Priya\" and \"who is out on Thursday\" have answers.",
		Capabilities: []string{"http:keka.com"},
		Config: []connectorkit.ConfigField{
			{
				Key:         "subdomain",
				Description: "Your Keka subdomain — the acme in https://acme.keka.com",
				Required:    true,
			},
			{Key: "client_id", Description: "From Keka → Settings → Integrations → Keka API", Required: true},
			{Key: "client_secret", Description: "The API client's secret", Required: true, Secret: true},
			{
				Key:         "api_key",
				Description: "The API key issued alongside the client — Keka requires all three",
				Required:    true,
				Secret:      true,
			},
		},
	}
}

// Auth is a token exchange, not a static key.
//
// Declared as AuthAPIKey because what the OPERATOR supplies is a key; the
// access token is minted from it per call and never stored. Declaring OAuth2
// would make KARMAX try to run a browser flow that Keka does not offer here.
func (c *Connector) Auth() connectorkit.AuthMethod {
	return connectorkit.AuthMethod{Kind: connectorkit.AuthAPIKey, APIKeyField: "api_key"}
}

func base(cr connectorkit.Credentials) (string, error) {
	sub := strings.TrimSpace(cr.Get("subdomain"))
	if sub == "" {
		return "", fmt.Errorf("keka: not configured — set subdomain to the acme in https://acme.keka.com")
	}
	// People paste the whole URL. It is unambiguous, so accept it — but peel it
	// in the order the parts actually appear, outermost first. Stripping
	// ".keka.com" before "/api/v1" leaves the host intact and re-appends the
	// domain, producing acme.keka.com.keka.com.
	sub = strings.TrimPrefix(strings.TrimPrefix(sub, "https://"), "http://")
	sub = strings.TrimRight(sub, "/")
	sub = strings.TrimSuffix(sub, "/api/v1")
	sub = strings.TrimRight(sub, "/")
	sub = strings.TrimSuffix(sub, ".keka.com")
	if sub == "" {
		return "", fmt.Errorf("keka: subdomain looks empty after trimming")
	}
	return "https://" + sub + ".keka.com/api/v1", nil
}

// tokenCache holds minted access tokens.
//
// Keka's tokens last an hour and the exchange is rate limited, so minting one
// per tool call would spend the budget on authentication rather than on work.
var tokenCache sync.Map // cacheKey -> *cachedToken

type cachedToken struct {
	token   string
	expires time.Time
}

func accessToken(ctx context.Context, cr connectorkit.Credentials) (string, error) {
	id, secret, key := strings.TrimSpace(cr.Get("client_id")),
		strings.TrimSpace(cr.Get("client_secret")),
		strings.TrimSpace(cr.Get("api_key"))
	if id == "" || secret == "" || key == "" {
		return "", fmt.Errorf("keka: client_id, client_secret and api_key are all required")
	}

	cacheKey := id + "\x00" + key
	if v, ok := tokenCache.Load(cacheKey); ok {
		if t := v.(*cachedToken); time.Now().Before(t.expires) {
			return t.token, nil
		}
	}

	form := url.Values{
		"grant_type":    {"kekaapi"},
		"scope":         {"kekaapi"},
		"client_id":     {id},
		"client_secret": {secret},
		"api_key":       {key},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		loginHost+"/connect/token", strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	res, err := httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))

	if res.StatusCode != http.StatusOK {
		return "", fmt.Errorf("keka: could not obtain an access token (%d) — check client_id, "+
			"client_secret and api_key: %s", res.StatusCode, snippet(raw))
	}
	var body struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.Unmarshal(raw, &body); err != nil || body.AccessToken == "" {
		return "", fmt.Errorf("keka: the token response was not what was expected: %s", snippet(raw))
	}

	ttl := time.Duration(body.ExpiresIn) * time.Second
	if ttl <= 0 {
		ttl = time.Hour
	}
	// Expire early. A token that dies mid-request produces a 401 that looks
	// like bad credentials rather than an expiry.
	tokenCache.Store(cacheKey, &cachedToken{token: body.AccessToken, expires: time.Now().Add(ttl - time.Minute)})
	return body.AccessToken, nil
}

func call(ctx context.Context, cr connectorkit.Credentials, path string, query url.Values, out any) error {
	root, err := base(cr)
	if err != nil {
		return err
	}
	tok, err := accessToken(ctx, cr)
	if err != nil {
		return err
	}

	full := root + path
	if len(query) > 0 {
		full += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, full, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Accept", "application/json")

	res, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(res.Body, 8<<20))

	switch {
	case res.StatusCode == http.StatusUnauthorized:
		// Drop the cached token: the usual cause is an expiry we mistimed, and
		// keeping it guarantees the retry fails the same way.
		tokenCache.Delete(strings.TrimSpace(cr.Get("client_id")) + "\x00" + strings.TrimSpace(cr.Get("api_key")))
		return fmt.Errorf("keka: unauthorized (401) — the API client may be disabled: %s", snippet(raw))
	case res.StatusCode == http.StatusForbidden:
		return fmt.Errorf("keka: forbidden (403) — the API client lacks access to this data: %s", snippet(raw))
	case res.StatusCode < 200 || res.StatusCode >= 300:
		return fmt.Errorf("keka: request failed (%d): %s", res.StatusCode, snippet(raw))
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(raw, out)
}

func snippet(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 300 {
		return s[:300] + "…"
	}
	return s
}

func (c *Connector) Health(ctx context.Context, cr connectorkit.Credentials) error {
	cctx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()

	var res struct {
		Data []json.RawMessage `json:"data"`
	}
	// One employee is the cheapest call that proves both the token exchange and
	// the tenant URL, which are the two things that are actually ever wrong.
	if err := call(cctx, cr, "/hris/employees", url.Values{"pageSize": {"1"}}, &res); err != nil {
		return err
	}
	return nil
}

func (c *Connector) Sources() []connectorkit.EventSource { return nil }
