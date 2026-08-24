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
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/MelloB1989/karmax/pkg/connectorkit"
)

// testKey is generated once and reused — RSA keygen is slow enough that every
// test doing it separately would make this package noticeably slow to run.
var testKeyPEM = func() string {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{
		Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key),
	}))
}()

func appCreds(cfg map[string]string) connectorkit.Credentials {
	base := map[string]string{
		"app_id": "12345", "app_private_key": testKeyPEM, "installation_id": "999",
	}
	for k, v := range cfg {
		base[k] = v
	}
	return creds(base)
}

func TestAppJWTIsWellFormedAndVerifiable(t *testing.T) {
	now := time.Now()
	tok, err := appJWT("12345", testKeyPEM, now)
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		t.Fatalf("a jwt has 3 parts, got %d", len(parts))
	}

	headerJSON, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		t.Fatal(err)
	}
	var h struct{ Alg, Typ string }
	if err := json.Unmarshal(headerJSON, &h); err != nil {
		t.Fatal(err)
	}
	if h.Alg != "RS256" && !strings.Contains(string(headerJSON), `"alg":"RS256"`) {
		t.Errorf("header = %s", headerJSON)
	}

	claimsJSON, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatal(err)
	}
	var claims struct {
		Iat, Exp int64
		Iss      string
	}
	if err := json.Unmarshal(claimsJSON, &claims); err != nil {
		t.Fatal(err)
	}
	if claims.Iss != "12345" {
		t.Errorf("iss = %q", claims.Iss)
	}
	if got := time.Unix(claims.Exp, 0).Sub(time.Unix(claims.Iat, 0)); got < 10*time.Minute || got > 11*time.Minute {
		t.Errorf("exp-iat = %s, want ~10m (plus the clock-drift margin)", got)
	}
	if claims.Exp-now.Unix() > int64(appJWTTTL.Seconds())+1 {
		t.Errorf("exp is more than 10 minutes out: exp=%d now=%d", claims.Exp, now.Unix())
	}

	// The signature must actually verify against the key's own public half —
	// proves this is a real RS256 signature, not just three base64 blobs.
	key, err := parseRSAPrivateKey(testKeyPEM)
	if err != nil {
		t.Fatal(err)
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		t.Fatal(err)
	}
	signingInput := parts[0] + "." + parts[1]
	sum := sha256.Sum256([]byte(signingInput))
	if err := rsa.VerifyPKCS1v15(&key.PublicKey, crypto.SHA256, sum[:], sig); err != nil {
		t.Errorf("signature does not verify: %v", err)
	}
}

func TestAppJWTRejectsAnUnparsableKey(t *testing.T) {
	if _, err := appJWT("1", "not a pem key", time.Now()); err == nil {
		t.Error("a garbage private key was accepted")
	}
}

// TestScopedMintNarrowsTheRequestBody is the one test in this file that
// matters most: it inspects the exact JSON body sent to the access_tokens
// endpoint and asserts the repository narrowing is actually there.
func TestScopedMintNarrowsTheRequestBody(t *testing.T) {
	var gotPath string
	var gotBody map[string]any
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"token": "ghs_scoped", "expires_at": time.Now().Add(time.Hour).Format(time.RFC3339),
		})
	}))
	defer srv.Close()
	defer setAPI(setAPI(srv.URL))

	tok, exp, err := MintScopedToken(context.Background(), appCreds(nil),
		[]string{"acme/api", "other-owner/web"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if tok != "ghs_scoped" {
		t.Errorf("token = %q", tok)
	}
	if exp.Before(time.Now()) {
		t.Errorf("expiry parsed wrong: %v", exp)
	}
	if gotPath != "/app/installations/999/access_tokens" {
		t.Errorf("path = %q", gotPath)
	}
	if !strings.HasPrefix(gotAuth, "Bearer ") {
		t.Errorf("no bearer jwt sent: %q", gotAuth)
	}

	repos, ok := gotBody["repositories"].([]any)
	if !ok || len(repos) != 2 {
		t.Fatalf("repositories in body = %#v", gotBody["repositories"])
	}
	// The endpoint scopes within one installation's account, so it wants bare
	// names — sending "owner/repo" here silently matches nothing.
	if repos[0] != "api" || repos[1] != "web" {
		t.Errorf("repositories not narrowed to bare names: %v", repos)
	}
	perms, ok := gotBody["permissions"].(map[string]any)
	if !ok || perms["contents"] != "write" || perms["pull_requests"] != "write" {
		t.Errorf("permissions in body = %#v", gotBody["permissions"])
	}
}

func TestScopedMintNeedsAtLeastOneRepo(t *testing.T) {
	if _, _, err := MintScopedToken(context.Background(), appCreds(nil), nil, nil); err == nil {
		t.Error("minting with no repositories was accepted")
	}
}

func TestScopedMintIsNeverCached(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		_ = json.NewEncoder(w).Encode(map[string]any{
			"token": "ghs_scoped", "expires_at": time.Now().Add(time.Hour).Format(time.RFC3339),
		})
	}))
	defer srv.Close()
	defer setAPI(setAPI(srv.URL))

	for i := 0; i < 3; i++ {
		if _, _, err := MintScopedToken(context.Background(), appCreds(nil), []string{"acme/api"}, nil); err != nil {
			t.Fatal(err)
		}
	}
	if calls != 3 {
		t.Errorf("scoped mint hit the network %d times, want 3 (no caching)", calls)
	}
}

// TestInstallationTokenIsCachedAndRefreshed exercises the path call() uses
// for the connector's own tools: cached until near expiry, then refreshed.
func TestInstallationTokenIsCachedAndRefreshed(t *testing.T) {
	resetTokenCache()

	calls := 0
	expires := time.Now().Add(time.Hour)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		body := map[string]any{}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if len(body) != 0 {
			t.Errorf("the unscoped connector path sent a narrowing body: %#v", body)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"token": "ghs_cached", "expires_at": expires.Format(time.RFC3339),
		})
	}))
	defer srv.Close()
	defer setAPI(setAPI(srv.URL))

	cr := appCreds(nil)
	tok1, err := cachedInstallationToken(context.Background(), cr)
	if err != nil {
		t.Fatal(err)
	}
	tok2, err := cachedInstallationToken(context.Background(), cr)
	if err != nil {
		t.Fatal(err)
	}
	if tok1 != "ghs_cached" || tok2 != "ghs_cached" {
		t.Errorf("tokens = %q, %q", tok1, tok2)
	}
	if calls != 1 {
		t.Errorf("hit the network %d times for two calls inside the token's life", calls)
	}

	// Force the cached entry to look like it is about to expire.
	tokenCacheMu.Lock()
	key := cr.Get("app_id") + ":" + cr.Get("installation_id")
	entry := tokenCache[key]
	entry.expiresAt = time.Now().Add(1 * time.Minute) // inside the 5-minute margin
	tokenCache[key] = entry
	tokenCacheMu.Unlock()

	if _, err := cachedInstallationToken(context.Background(), cr); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Errorf("a near-expiry token was not refreshed: %d calls", calls)
	}
}

func TestTokenForPicksAppAuthOverAPAT(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"token": "ghs_app", "expires_at": time.Now().Add(time.Hour).Format(time.RFC3339),
		})
	}))
	defer srv.Close()
	defer setAPI(setAPI(srv.URL))
	resetTokenCache()

	// Both a PAT and App fields are configured — App auth must win, since it
	// is what a scoped, short-lived credential actually requires.
	cr := appCreds(map[string]string{"token": "ghp_should_not_be_used"})
	got, err := tokenFor(context.Background(), cr)
	if err != nil {
		t.Fatal(err)
	}
	if got != "ghs_app" {
		t.Errorf("tokenFor = %q, want the app-minted token", got)
	}
}

func TestTokenForFallsBackToThePATWithNoAppConfig(t *testing.T) {
	got, err := tokenFor(context.Background(), creds(map[string]string{"token": "ghp_x"}))
	if err != nil || got != "ghp_x" {
		t.Errorf("token=%q err=%v", got, err)
	}
}

func TestPartialAppConfigFailsClearly(t *testing.T) {
	_, err := tokenFor(context.Background(), creds(map[string]string{"app_id": "1"}))
	if err == nil || !contains(err.Error(), "app auth needs") {
		t.Errorf("partial app config produced: %v", err)
	}
}

// TestHealthDistinguishesAppFailureModes covers the four cases §6 of the
// brief asks for: no credentials, a bad private key, an uninstalled app, and
// GitHub being unreachable.
func TestHealthDistinguishesAppFailureModes(t *testing.T) {
	c := New("")

	if err := c.Health(context.Background(), creds(nil)); err == nil || !contains(err.Error(), "no token configured") {
		t.Errorf("no credentials at all produced: %v", err)
	}

	if err := c.Health(context.Background(), creds(map[string]string{
		"app_id": "1", "app_private_key": "garbage", "installation_id": "1",
	})); err == nil || !contains(err.Error(), "private key") {
		t.Errorf("a bad private key produced: %v", err)
	}

	notFound := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer notFound.Close()
	defer setAPI(setAPI(notFound.URL))
	resetTokenCache()
	if err := c.Health(context.Background(), appCreds(nil)); err == nil || !contains(err.Error(), "may not be installed") {
		t.Errorf("an uninstalled app produced: %v", err)
	}
}

func TestHealthReportsGitHubUnreachable(t *testing.T) {
	resetTokenCache()
	defer setAPI(setAPI("http://127.0.0.1:1")) // nothing listens here
	c := New("")
	err := c.Health(context.Background(), appCreds(nil))
	if err == nil || !contains(err.Error(), "could not reach github") {
		t.Errorf("an unreachable github produced: %v", err)
	}
}

func resetTokenCache() {
	tokenCacheMu.Lock()
	tokenCache = map[string]cachedToken{}
	tokenCacheMu.Unlock()
}
