// Package google connects KARMAX to Google Workspace, one employee at a time.
//
// The shape is the one an org actually needs: the company registers ONE OAuth
// app — client id and secret, configured once by an admin — and then every
// employee authorises their own Google account against it. KARMAX stores a
// token per person and picks the right one from whoever is being acted for.
//
// This is why the connector is PerUser. A single shared token would mean the
// whole company reading one inbox, and "what's on my calendar tomorrow?"
// returning somebody else's day. That is not a permissions bug to tighten
// later; it is a confidently wrong answer about someone's private data.
//
// Scopes cover Gmail, Calendar, Drive and Chat because all four were asked
// for. They are requested together at consent time: Google shows the employee
// exactly what is being granted, and asking four separate times for four
// screens is worse for the person deciding.
package google

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/MelloB1989/karmax/pkg/connectorkit"
)

// Endpoints are vars so tests can point them at a stub.
var (
	authEndpoint  = "https://accounts.google.com/o/oauth2/v2/auth"
	tokenEndpoint = "https://oauth2.googleapis.com/token"
	gmailAPI      = "https://gmail.googleapis.com"
	calendarAPI   = "https://www.googleapis.com/calendar/v3"
	driveAPI      = "https://www.googleapis.com/drive/v3"
	chatAPI       = "https://chat.googleapis.com"
	userinfoAPI   = "https://www.googleapis.com/oauth2/v2/userinfo"
)

var httpClient = &http.Client{Timeout: 40 * time.Second}

// Scopes requested at consent.
//
// gmail.modify rather than gmail.readonly: sending and replying were both
// asked for, and modify is what lets a draft be created and a thread marked
// read. drive.readonly rather than drive: an agent that can DELETE company
// documents is a different conversation from one that can read them.
var Scopes = []string{
	"openid",
	"https://www.googleapis.com/auth/userinfo.email",
	"https://www.googleapis.com/auth/gmail.modify",
	"https://www.googleapis.com/auth/gmail.send",
	"https://www.googleapis.com/auth/calendar.events",
	"https://www.googleapis.com/auth/calendar.readonly",
	"https://www.googleapis.com/auth/drive.readonly",
	"https://www.googleapis.com/auth/chat.messages",
	"https://www.googleapis.com/auth/chat.spaces.readonly",
}

// Connector is one Google Workspace org.
type Connector struct{}

func New() *Connector { return &Connector{} }

func (c *Connector) Manifest() connectorkit.Manifest {
	return connectorkit.Manifest{
		ID:   "google",
		Name: "Google Workspace",
		Description: "Gmail, Calendar, Drive and Chat — as each employee. The org registers one " +
			"OAuth app; everyone connects their own account against it, and the agent acts as " +
			"whoever it is helping.",
		// PerUser is the whole design: without it one person's token would
		// serve everybody's requests.
		PerUser:      true,
		Capabilities: []string{"http:googleapis.com", "http:accounts.google.com"},
		Config: []connectorkit.ConfigField{
			{
				Key:         "client_id",
				Description: "OAuth client ID from console.cloud.google.com → APIs & Services → Credentials",
				Required:    true,
			},
			{
				Key:         "client_secret",
				Description: "OAuth client secret for the same client",
				Required:    true,
				Secret:      true,
			},
			{
				Key: "hosted_domain",
				Description: "Restrict sign-in to one Workspace domain, e.g. acme.com — " +
					"leave blank to allow any Google account",
			},
		},
	}
}

func (c *Connector) Auth() connectorkit.AuthMethod {
	return connectorkit.AuthMethod{
		Kind: connectorkit.AuthOAuth2,
		OAuth2: &connectorkit.OAuth2Config{
			AuthURL:     authEndpoint,
			TokenURL:    tokenEndpoint,
			Scopes:      Scopes,
			ClientIDKey: "client_id",
			SecretKey:   "client_secret",
		},
	}
}

// AuthCodeURL builds the consent URL one employee is sent to.
//
// access_type=offline and prompt=consent together are what produce a REFRESH
// token. Google issues one only on the first consent for a given client and
// user, so without prompt=consent a person who has authorised this app before
// gets an access token that dies in an hour and never comes back — which
// presents later as "it worked yesterday".
func AuthCodeURL(cr connectorkit.Credentials, redirectURI, state string) (string, error) {
	clientID := strings.TrimSpace(cr.Get("client_id"))
	if clientID == "" {
		return "", fmt.Errorf("google: no client_id configured — an admin has to set up the OAuth app first")
	}
	if redirectURI == "" {
		return "", fmt.Errorf("google: no redirect URI — set console.public_url so this server knows its own address")
	}

	q := url.Values{
		"client_id":     {clientID},
		"redirect_uri":  {redirectURI},
		"response_type": {"code"},
		"scope":         {strings.Join(Scopes, " ")},
		"state":         {state},
		"access_type":   {"offline"},
		"prompt":        {"consent"},
		// include_granted_scopes keeps previously granted scopes when this app
		// asks for more later, instead of silently dropping them.
		"include_granted_scopes": {"true"},
	}
	if hd := strings.TrimSpace(cr.Get("hosted_domain")); hd != "" {
		q.Set("hd", hd)
	}
	return authEndpoint + "?" + q.Encode(), nil
}

// TokenSet is what Google returns from the token endpoint.
type TokenSet struct {
	AccessToken  string
	RefreshToken string
	Expiry       time.Time
	Scopes       []string
	Email        string
	Name         string
}

// Exchange turns an authorisation code into tokens.
func Exchange(ctx context.Context, cr connectorkit.Credentials, code, redirectURI string) (TokenSet, error) {
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {redirectURI},
		"client_id":     {strings.TrimSpace(cr.Get("client_id"))},
		"client_secret": {strings.TrimSpace(cr.Get("client_secret"))},
	}
	ts, err := postToken(ctx, form)
	if err != nil {
		return TokenSet{}, err
	}
	if ts.RefreshToken == "" {
		// Worth saying out loud rather than storing a connection that dies in
		// an hour and looks fine until it does.
		return ts, fmt.Errorf("google: no refresh token was issued — the account has probably " +
			"authorised this app before; revoke it at myaccount.google.com/permissions and connect again")
	}

	email, err := whoAmI(ctx, ts.AccessToken)
	if err == nil {
		ts.Email = email
	}
	return ts, nil
}

// Refresh renews an access token from a refresh token.
func Refresh(ctx context.Context, cr connectorkit.Credentials, refreshToken string) (TokenSet, error) {
	if strings.TrimSpace(refreshToken) == "" {
		return TokenSet{}, fmt.Errorf("google: no refresh token stored — the account must be reconnected")
	}
	return postToken(ctx, url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refreshToken},
		"client_id":     {strings.TrimSpace(cr.Get("client_id"))},
		"client_secret": {strings.TrimSpace(cr.Get("client_secret"))},
	})
}

func postToken(ctx context.Context, form url.Values) (TokenSet, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenEndpoint,
		strings.NewReader(form.Encode()))
	if err != nil {
		return TokenSet{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	res, err := httpClient.Do(req)
	if err != nil {
		return TokenSet{}, err
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))

	var body struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int    `json:"expires_in"`
		Scope        string `json:"scope"`
		Error        string `json:"error"`
		ErrorDesc    string `json:"error_description"`
	}
	_ = json.Unmarshal(raw, &body)

	if res.StatusCode != http.StatusOK || body.AccessToken == "" {
		detail := strings.TrimSpace(body.ErrorDesc)
		if detail == "" {
			detail = strings.TrimSpace(body.Error)
		}
		if detail == "" {
			detail = snippet(raw)
		}
		if body.Error == "invalid_grant" {
			return TokenSet{}, fmt.Errorf("google: the grant is no longer valid (%s) — the person "+
				"revoked access, changed their password, or the app was removed; they need to connect again", detail)
		}
		return TokenSet{}, fmt.Errorf("google: token request failed (%d): %s", res.StatusCode, detail)
	}

	ttl := time.Duration(body.ExpiresIn) * time.Second
	if ttl <= 0 {
		ttl = time.Hour
	}
	return TokenSet{
		AccessToken:  body.AccessToken,
		RefreshToken: body.RefreshToken,
		Expiry:       time.Now().Add(ttl),
		Scopes:       strings.Fields(body.Scope),
	}, nil
}

// whoAmI names the account that just authorised, so the console can show a
// person which mailbox they connected rather than an opaque "connected".
func whoAmI(ctx context.Context, accessToken string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, userinfoAPI, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)

	res, err := httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return "", fmt.Errorf("google: userinfo returned %d", res.StatusCode)
	}
	var body struct {
		Email string `json:"email"`
	}
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		return "", err
	}
	return body.Email, nil
}

func (c *Connector) Health(ctx context.Context, cr connectorkit.Credentials) error {
	// Without an acting employee there is nothing to check: the org's OAuth app
	// config is not itself a credential, and pretending otherwise would report
	// healthy for a connector nobody can use.
	if cr.AccessToken == "" {
		if strings.TrimSpace(cr.Get("client_id")) == "" {
			return fmt.Errorf("google: no OAuth app configured yet")
		}
		return fmt.Errorf("google: the OAuth app is configured, but this is a per-person " +
			"connector — each employee connects their own account, and health is checked as them")
	}

	cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	if _, err := whoAmI(cctx, cr.AccessToken); err != nil {
		return err
	}
	return nil
}

func (c *Connector) Sources() []connectorkit.EventSource { return nil }

func snippet(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 300 {
		return s[:300] + "…"
	}
	return s
}

// Signing in is a different request from connecting a mailbox.
//
// SignInScopes asks only who you are. Reusing the full connector scopes would
// put "read and send your email, read your Drive" on a consent screen whose
// entire job is to establish an identity — which is both alarming and, for
// someone who only ever needs to sign in, a grant they should never have been
// asked for.
var SignInScopes = []string{
	"openid",
	"https://www.googleapis.com/auth/userinfo.email",
	"https://www.googleapis.com/auth/userinfo.profile",
}

// AuthCodeURLForSignIn builds the consent URL for signing in to the console.
//
// No access_type=offline and no prompt=consent: a sign-in needs one access
// token for one call to userinfo, and asking for offline access would request
// a refresh token nothing will ever use.
//
// prompt=select_account so somebody signed into two Google accounts is asked
// which one, rather than being silently taken in as whichever is first.
func AuthCodeURLForSignIn(cr connectorkit.Credentials, redirectURI, state string) (string, error) {
	clientID := strings.TrimSpace(cr.Get("client_id"))
	if clientID == "" {
		return "", fmt.Errorf("google: no client_id configured")
	}
	if redirectURI == "" {
		return "", fmt.Errorf("google: no redirect URI — set console.public_url")
	}

	q := url.Values{
		"client_id":     {clientID},
		"redirect_uri":  {redirectURI},
		"response_type": {"code"},
		"scope":         {strings.Join(SignInScopes, " ")},
		"state":         {state},
		"prompt":        {"select_account"},
	}
	if hd := strings.TrimSpace(cr.Get("hosted_domain")); hd != "" {
		// A hint to the consent screen, not a guarantee: the caller must still
		// check the domain of whatever address comes back.
		q.Set("hd", hd)
	}
	return authEndpoint + "?" + q.Encode(), nil
}

// ExchangeIdentity turns a sign-in code into the account's identity.
//
// Unlike Exchange it does NOT insist on a refresh token. Signing in needs one
// access token for one call; demanding offline access would refuse anybody who
// has authorised this app before, which is everybody after their first login.
func ExchangeIdentity(ctx context.Context, cr connectorkit.Credentials, code, redirectURI string) (TokenSet, error) {
	ts, err := postToken(ctx, url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {redirectURI},
		"client_id":     {strings.TrimSpace(cr.Get("client_id"))},
		"client_secret": {strings.TrimSpace(cr.Get("client_secret"))},
	})
	if err != nil {
		return TokenSet{}, err
	}

	email, name, verified, err := identity(ctx, ts.AccessToken)
	if err != nil {
		return TokenSet{}, err
	}
	if !verified {
		// An unverified address is one Google has not confirmed the holder
		// controls. Matching an account against it would let someone sign in as
		// a colleague by claiming their address.
		return TokenSet{}, fmt.Errorf("google: %s is not a verified address on that account", email)
	}
	ts.Email, ts.Name = email, name
	return ts, nil
}

// identity reads who an access token belongs to.
func identity(ctx context.Context, accessToken string) (email, name string, verified bool, err error) {
	req, rerr := http.NewRequestWithContext(ctx, http.MethodGet, userinfoAPI, nil)
	if rerr != nil {
		return "", "", false, rerr
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)

	res, derr := httpClient.Do(req)
	if derr != nil {
		return "", "", false, derr
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return "", "", false, fmt.Errorf("google: userinfo returned %d", res.StatusCode)
	}

	var body struct {
		Email         string `json:"email"`
		VerifiedEmail *bool  `json:"verified_email"`
		Name          string `json:"name"`
	}
	if derr := json.NewDecoder(res.Body).Decode(&body); derr != nil {
		return "", "", false, derr
	}
	// Absent means Google did not say. Treated as verified because the v2
	// userinfo endpoint omits the field for Workspace accounts, where the
	// domain administrator owns the address anyway.
	ok := body.VerifiedEmail == nil || *body.VerifiedEmail
	return body.Email, body.Name, ok, nil
}
