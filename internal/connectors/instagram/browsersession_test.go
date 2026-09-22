package instagram

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/MelloB1989/karmax/pkg/connectorkit"
)

func TestConfiguredCountsASignedInBrowser(t *testing.T) {
	c := New()
	empty := connectorkit.Credentials{Config: map[string]string{}}
	if c.Configured(empty) {
		t.Fatal("nothing stored and no browser: that is not configured")
	}

	c.SetBrowserSession(func(context.Context) (string, error) { return "", nil })
	if c.Configured(empty) {
		t.Error("a browser nobody is signed into must not count")
	}

	c.SetBrowserSession(func(context.Context) (string, error) { return "", errors.New("not running") })
	if c.Configured(empty) {
		t.Error("a browser that cannot be asked must not count")
	}

	c.SetBrowserSession(func(context.Context) (string, error) { return "1234%3Aabc", nil })
	if !c.Configured(empty) {
		t.Error("signing in in the shared browser is how the desktop app connects Instagram")
	}
}

func TestConfiguredStillCountsStoredCredentials(t *testing.T) {
	c := New()
	for _, cfg := range []map[string]string{
		{"sessionid": "1234%3Aabc"},
		{"username": "someone", "password": "pw"},
	} {
		if !c.Configured(connectorkit.Credentials{Config: cfg}) {
			t.Errorf("stored %v must count as configured", cfg)
		}
	}
}

func TestEnsureWithNothingSaysToSignInInTheBrowser(t *testing.T) {
	t.Setenv("KARMAX_ENABLE_INSTAGRAM", "true")
	c := New()
	c.h = testHelper(t)
	asked := false
	c.SetBrowserSession(func(context.Context) (string, error) { asked = true; return "", nil })

	_, err := c.ensure(context.Background(), connectorkit.Credentials{Config: map[string]string{}})
	if err == nil {
		t.Fatal("with nothing to sign in with, ensure must fail")
	}
	if !asked {
		t.Error("with nothing stored, the shared browser must be asked")
	}
	if !strings.Contains(err.Error(), "shares with you") {
		t.Errorf("the error must point at the shared browser, got: %v", err)
	}
}

func TestStoredCredentialsAreUsedBeforeTheBrowser(t *testing.T) {
	t.Setenv("KARMAX_ENABLE_INSTAGRAM", "true")
	c := New()
	c.h = testHelper(t)
	asked := false
	c.SetBrowserSession(func(context.Context) (string, error) { asked = true; return "", nil })

	// A seed that cannot decode fails before anything reaches Instagram.
	_, err := c.ensure(context.Background(), connectorkit.Credentials{Config: map[string]string{
		"username": "someone", "password": "pw", "totp_seed": "not base32 0189",
	}})
	if err == nil || !strings.Contains(err.Error(), "2FA seed") {
		t.Fatalf("want the seed error, got: %v", err)
	}
	if asked {
		t.Error("a stored password was set up on purpose; the browser must not override it")
	}
}
