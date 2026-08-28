package github

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/MelloB1989/karmax/pkg/connectorkit"
)

// GitHub App auth. A PAT is one long-lived credential with a human's full
// permissions; an installation token is short-lived and can be minted narrow
// to a handful of repos, which is what "an agent gets real write access" is
// supposed to mean. The private key never touches disk here — it lives in
// the credential store and passes through only as a config string.

// appJWTTTL is GitHub's own ceiling for an App JWT; anything longer is refused.
const appJWTTTL = 10 * time.Minute

// clockDrift backdates iat, per GitHub's own recommendation for a server
// whose clock runs a little ahead of GitHub's.
const clockDrift = 60 * time.Second

// appJWT signs the RS256 JWT an App uses to ask for an installation token.
func appJWT(appID, privateKeyPEM string, now time.Time) (string, error) {
	key, err := parseRSAPrivateKey(privateKeyPEM)
	if err != nil {
		return "", fmt.Errorf("github: app private key: %w", err)
	}
	claims, err := json.Marshal(map[string]any{
		"iat": now.Add(-clockDrift).Unix(),
		"exp": now.Add(appJWTTTL).Unix(),
		"iss": appID,
	})
	if err != nil {
		return "", err
	}

	signingInput := base64URL([]byte(`{"alg":"RS256","typ":"JWT"}`)) + "." + base64URL(claims)
	sum := sha256.Sum256([]byte(signingInput))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sum[:])
	if err != nil {
		return "", fmt.Errorf("github: could not sign the app jwt: %w", err)
	}
	return signingInput + "." + base64URL(sig), nil
}

func base64URL(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// parseRSAPrivateKey accepts either PEM form GitHub hands out — PKCS1 is what
// the App settings page downloads, PKCS8 is what most conversions produce.
func parseRSAPrivateKey(pemStr string) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode([]byte(pemStr))
	if block == nil {
		return nil, fmt.Errorf("not a PEM-encoded key")
	}
	if key, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return key, nil
	}
	generic, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("unreadable key: %w", err)
	}
	key, ok := generic.(*rsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("not an RSA key")
	}
	return key, nil
}

// usesAppAuth reports whether to authenticate as a GitHub App.
//
// The RECORDED method wins when there is one. Deciding purely by which fields
// are non-empty means a half-finished App attempt — an app id typed in and
// abandoned, with no private key — silently takes precedence over a personal
// access token that works, and every call then fails complaining about a key
// the operator never meant to use.
//
// Falls back to the old inference for credentials saved before the method was
// recorded, so existing installs keep behaving exactly as they did.
func usesAppAuth(cr connectorkit.Credentials) bool {
	switch strings.TrimSpace(cr.Get("auth_method")) {
	case "app":
		return true
	case "pat":
		return false
	}
	appTouched := cr.Get("app_id") != "" || cr.Get("app_private_key") != "" || cr.Get("installation_id") != ""
	if !appTouched {
		return false
	}

	// An App config that cannot possibly work, next to a token that can, means
	// the token. Choosing the App there produces "app auth needs
	// app_private_key" about a setup the operator abandoned, while a working
	// credential sits unused beside it.
	//
	// With no token to fall back on, the App is still the answer: the error
	// then names the field they actually need to finish.
	appComplete := cr.Get("app_id") != "" && cr.Get("app_private_key") != ""
	hasToken := cr.AccessToken != "" || cr.Get("token") != ""
	return appComplete || !hasToken
}

func requireAppConfig(cr connectorkit.Credentials) error {
	var missing []string
	for _, f := range []string{"app_id", "app_private_key", "installation_id"} {
		if cr.Get(f) == "" {
			missing = append(missing, f)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("github: app auth needs %s", strings.Join(missing, ", "))
	}
	return nil
}

// tokenFor is what call() actually authenticates with: App auth when the
// operator configured one, the stored PAT otherwise. The PAT path is
// untouched by any of this — same fields, same error, same behaviour.
func tokenFor(ctx context.Context, cr connectorkit.Credentials) (string, error) {
	if usesAppAuth(cr) {
		if err := requireAppConfig(cr); err != nil {
			return "", err
		}
		return cachedInstallationToken(ctx, cr)
	}
	token := cr.AccessToken
	if token == "" {
		token = cr.Get("token")
	}
	if token == "" {
		return "", fmt.Errorf("github: no token configured")
	}
	return token, nil
}

// defaultSandboxPermissions is the least a coding agent needs: push a branch,
// open a pull request, and read the repository to do either.
var defaultSandboxPermissions = map[string]string{
	"contents":      "write",
	"pull_requests": "write",
	"metadata":      "read",
}

type installationTokenResponse struct {
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expires_at"`
}

// installationToken exchanges an App JWT for an installation access token.
// repos and permissions, when set, narrow it — the entire reason App auth
// exists over a PAT — so getting this request body right matters more than
// anything else in this file.
func installationToken(ctx context.Context, cr connectorkit.Credentials, repos []string, permissions map[string]string) (string, time.Time, error) {
	if err := requireAppConfig(cr); err != nil {
		return "", time.Time{}, err
	}
	jwtStr, err := appJWT(cr.Get("app_id"), cr.Get("app_private_key"), time.Now())
	if err != nil {
		return "", time.Time{}, err
	}

	reqBody := map[string]any{}
	if len(repos) > 0 {
		reqBody["repositories"] = bareRepoNames(repos)
	}
	if len(permissions) > 0 {
		reqBody["permissions"] = permissions
	}
	payload, err := json.Marshal(reqBody)
	if err != nil {
		return "", time.Time{}, err
	}

	installationID := cr.Get("installation_id")
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		api+"/app/installations/"+installationID+"/access_tokens", strings.NewReader(string(payload)))
	if err != nil {
		return "", time.Time{}, err
	}
	req.Header.Set("Authorization", "Bearer "+jwtStr)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "karmax-connector/1")
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("github: could not reach github: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	switch {
	case resp.StatusCode == http.StatusNotFound:
		return "", time.Time{}, fmt.Errorf("github: installation %s not found — the app may not be installed there", installationID)
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return "", time.Time{}, fmt.Errorf("github: the app jwt was rejected — check app_id matches app_private_key: %s", strings.TrimSpace(string(raw)))
	case resp.StatusCode < 200 || resp.StatusCode > 299:
		return "", time.Time{}, fmt.Errorf("github: token exchange failed: %s %s", resp.Status, strings.TrimSpace(string(raw)))
	}

	var out installationTokenResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", time.Time{}, fmt.Errorf("github: undecodable token exchange response: %w", err)
	}
	return out.Token, out.ExpiresAt, nil
}

// bareRepoNames strips any "owner/" prefix. The access_tokens endpoint scopes
// within one installation's account, so it wants repository names alone —
// sending "owner/repo" there silently matches nothing.
func bareRepoNames(repos []string) []string {
	out := make([]string, 0, len(repos))
	for _, r := range repos {
		r = strings.TrimSpace(r)
		if r == "" {
			continue
		}
		if i := strings.LastIndex(r, "/"); i >= 0 {
			r = r[i+1:]
		}
		out = append(out, r)
	}
	return out
}

// tokenCacheMargin is how long before expiry a cached token is treated as
// gone, so a request never starts with a token that expires mid-flight.
const tokenCacheMargin = 5 * time.Minute

type cachedToken struct {
	token     string
	expiresAt time.Time
}

// tokenCache is keyed by app+installation rather than by account name, since
// that is what actually identifies which installation token this is — two
// accounts pointed at the same installation should share one.
var (
	tokenCacheMu sync.Mutex
	tokenCache   = map[string]cachedToken{}
)

// cachedInstallationToken is the unscoped token call() authenticates the
// connector's own tools with, refreshed transparently a few minutes before
// it would otherwise expire. Never used for a sandbox run — see MintScopedToken.
func cachedInstallationToken(ctx context.Context, cr connectorkit.Credentials) (string, error) {
	key := cr.Get("app_id") + ":" + cr.Get("installation_id")

	tokenCacheMu.Lock()
	if t, ok := tokenCache[key]; ok && time.Now().Before(t.expiresAt.Add(-tokenCacheMargin)) {
		tokenCacheMu.Unlock()
		return t.token, nil
	}
	tokenCacheMu.Unlock()

	token, expiresAt, err := installationToken(ctx, cr, nil, nil)
	if err != nil {
		return "", err
	}
	tokenCacheMu.Lock()
	tokenCache[key] = cachedToken{token: token, expiresAt: expiresAt}
	tokenCacheMu.Unlock()
	return token, nil
}

// MintScopedToken is what the sandbox driver calls: an installation token
// narrowed to exactly the repositories one run may touch, with default
// permissions if the caller does not need something narrower still. Never
// cached — each run gets its own mint scoped to its own repo list, so one
// run's token cannot end up handed to another by accident.
func MintScopedToken(ctx context.Context, cr connectorkit.Credentials, repos []string, permissions map[string]string) (string, time.Time, error) {
	if len(repos) == 0 {
		return "", time.Time{}, fmt.Errorf("github: a scoped token needs at least one repository")
	}
	if permissions == nil {
		permissions = defaultSandboxPermissions
	}
	return installationToken(ctx, cr, repos, permissions)
}

// NewRepoTokenMinter adapts MintScopedToken to runtime.RepoTokenMinter's
// shape (func(ctx, repo) (string, error)) without this package importing the
// runtime — creds is resolved on every call, not captured once, so a rotated
// private key takes effect without a restart.
func NewRepoTokenMinter(creds func(ctx context.Context) (connectorkit.Credentials, error)) func(ctx context.Context, repo string) (string, error) {
	return func(ctx context.Context, repo string) (string, error) {
		cr, err := creds(ctx)
		if err != nil {
			return "", err
		}
		token, _, err := MintScopedToken(ctx, cr, []string{repo}, nil)
		return token, err
	}
}
