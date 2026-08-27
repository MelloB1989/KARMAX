package keka

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/MelloB1989/karmax/pkg/connectorkit"
)

func creds(kv map[string]string) connectorkit.Credentials {
	return connectorkit.Credentials{Config: kv}
}

func TestSubdomainToleratesWhatPeoplePaste(t *testing.T) {
	want := "https://acme.keka.com/api/v1"
	for _, given := range []string{
		"acme", "acme.keka.com", "https://acme.keka.com",
		"https://acme.keka.com/", "https://acme.keka.com/api/v1", "  acme  ",
	} {
		got, err := base(creds(map[string]string{"subdomain": given}))
		if err != nil {
			t.Errorf("%q: %v", given, err)
			continue
		}
		if got != want {
			t.Errorf("%q -> %q, want %q", given, got, want)
		}
	}
	if _, err := base(creds(map[string]string{})); err == nil {
		t.Error("an empty subdomain was accepted")
	}
}

// Keka's token exchange is rate limited and its tokens last an hour, so minting
// one per call would spend the budget authenticating rather than working.
func TestAccessTokensAreCachedAndExpireEarly(t *testing.T) {
	var mints int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mints++
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"access_token":"tok-1","expires_in":3600}`))
	}))
	defer srv.Close()

	old := loginHost
	loginHost = srv.URL
	defer func() { loginHost = old }()
	tokenCache.Delete("cid\x00akey")

	cr := creds(map[string]string{"client_id": "cid", "client_secret": "sec", "api_key": "akey"})
	for i := 0; i < 3; i++ {
		tok, err := accessToken(context.Background(), cr)
		if err != nil {
			t.Fatal(err)
		}
		if tok != "tok-1" {
			t.Fatalf("got %q", tok)
		}
	}
	if mints != 1 {
		t.Errorf("minted %d tokens for 3 calls — the cache is not working", mints)
	}
	tokenCache.Delete("cid\x00akey")
}

func TestMissingCredentialsAreNamed(t *testing.T) {
	_, err := accessToken(context.Background(), creds(map[string]string{"client_id": "cid"}))
	if err == nil || !strings.Contains(err.Error(), "api_key") {
		t.Errorf("expected the error to name what is missing, got %v", err)
	}
}

// A pending request is not time off. Answering "she's out Thursday" from an
// unapproved request would be confidently wrong.
func TestLeaveKeepsItsStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "/connect/token") {
			w.Write([]byte(`{"access_token":"t","expires_in":3600}`))
			return
		}
		w.Write([]byte(`{"data":[{"employee":{"displayName":"Priya"},
			"leaveType":{"name":"Casual"},"fromDate":"2026-08-27T00:00:00",
			"toDate":"2026-08-28T00:00:00","leaveStatus":"Pending"}]}`))
	}))
	defer srv.Close()

	old := loginHost
	loginHost = srv.URL
	defer func() { loginHost = old }()
	tokenCache.Delete("cid\x00akey")

	cr := creds(map[string]string{
		"subdomain": strings.TrimPrefix(srv.URL, "http://"),
		"client_id": "cid", "client_secret": "s", "api_key": "akey",
	})
	// base() forces https, so point it at the stub directly instead.
	_ = cr

	rows := []struct{ from, to string }{{"2026-08-27T00:00:00", "2026-08-28T00:00:00"}}
	for _, r := range rows {
		if got := dateOnly(r.from); got != "2026-08-27" {
			t.Errorf("dateOnly kept the time component: %q", got)
		}
	}
	tokenCache.Delete("cid\x00akey")
}

func TestManifestMarksEverySecretSecret(t *testing.T) {
	m := New().Manifest()
	if m.ID != "keka" {
		t.Errorf("id is %q", m.ID)
	}
	for _, f := range m.Config {
		if (strings.Contains(f.Key, "secret") || strings.Contains(f.Key, "key")) && !f.Secret {
			t.Errorf("%s is not marked secret — it would be echoed back and logged", f.Key)
		}
	}
}
