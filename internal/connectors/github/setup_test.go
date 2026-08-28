package github

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"strings"
	"testing"

	"github.com/MelloB1989/karmax/pkg/connectorkit"
)

func cr(kv map[string]string) connectorkit.Credentials {
	return connectorkit.Credentials{Config: kv}
}

// The step with no field attached. A guide generated from the config list can
// never mention it, and it is the one people miss.
func TestSetupTellsYouToInstallTheApp(t *testing.T) {
	steps := New("").SetupSteps(cr(map[string]string{}), "")

	var found bool
	for _, s := range steps {
		if strings.Contains(strings.ToLower(s.Title), "install") {
			found = true
			if !strings.Contains(s.Body, "access to nothing") {
				t.Error("the install step does not explain what happens if you skip it")
			}
			if s.URL == "" {
				t.Error("the install step has no link to follow")
			}
		}
	}
	if !found {
		t.Fatal("setup never mentions installing the App on repositories")
	}
}

// Once the slug is known the link can go straight to the install page; without
// it, it must go somewhere real rather than to a URL that 404s.
func TestInstallLinkIsDirectOnlyWhenItCanBe(t *testing.T) {
	withSlug := installURL(cr(map[string]string{"app_slug": "karmax-bot"}))
	if withSlug != "https://github.com/apps/karmax-bot/installations/new" {
		t.Errorf("with a slug the link should be direct, got %q", withSlug)
	}
	without := installURL(cr(map[string]string{}))
	if strings.Contains(without, "/apps//") {
		t.Errorf("an empty slug produced a broken URL: %q", without)
	}
	if without != "https://github.com/settings/apps" {
		t.Errorf("without a slug it should fall back to the app list, got %q", without)
	}
}

// A half-finished setup should say which half.
func TestStepsReportWhatIsAlreadyDone(t *testing.T) {
	partial := New("").SetupSteps(cr(map[string]string{
		"app_id": "123", "app_private_key": "-----BEGIN RSA PRIVATE KEY-----",
	}), "")

	var appDone, installDone *bool
	for _, s := range partial {
		if strings.Contains(s.Title, "Create a GitHub App") {
			appDone = s.Done
		}
		if strings.Contains(strings.ToLower(s.Title), "install the app") {
			installDone = s.Done
		}
	}
	if appDone == nil || !*appDone {
		t.Error("the app step should be marked done once id and key are saved")
	}
	if installDone == nil || *installDone {
		t.Error("the install step should NOT be marked done without an installation_id")
	}
}

// An App installed nowhere is a working credential with access to zero repos.
// The health check has to say that, not just fail.
func TestNotInstalledIsExplained(t *testing.T) {
	msg := describeInstallations(nil)
	if !strings.Contains(msg, "not installed on any repository") {
		t.Errorf("unhelpful message for zero installations: %q", msg)
	}

	msg = describeInstallations([]installation{
		{ID: 42, RepositorySelection: "selected", Account: struct {
			Login string `json:"login"`
		}{Login: "acme"}},
	})
	// The installation id is otherwise only visible in the URL bar during
	// installation, so printing it is the point.
	if !strings.Contains(msg, "42") || !strings.Contains(msg, "acme") {
		t.Errorf("the message omits the id or the account: %q", msg)
	}
}

// A half-finished App attempt — an app id typed in and abandoned — used to
// take precedence over a personal access token that works, and every call then
// failed complaining about a private key the operator never meant to use.
func TestTheRecordedMethodBeatsLeftoverFields(t *testing.T) {
	halfFinished := cr(map[string]string{
		"auth_method": "pat",
		"token":       "ghp_works",
		"app_id":      "4736085", // typed in, then abandoned
	})
	if usesAppAuth(halfFinished) {
		t.Error("a leftover app id overrode the recorded token method")
	}

	// And the reverse: a recorded App method is honoured even with a token
	// still sitting there.
	both := cr(map[string]string{
		"auth_method": "app", "token": "ghp_old",
		"app_id": "1", "app_private_key": "KEY", "installation_id": "2",
	})
	if !usesAppAuth(both) {
		t.Error("a recorded App method was ignored")
	}
}

// Credentials saved before the method was recorded must keep behaving exactly
// as they did.
func TestInferenceStillWorksWithNoRecordedMethod(t *testing.T) {
	if !usesAppAuth(cr(map[string]string{"app_id": "1"})) {
		t.Error("an app id no longer implies App auth when nothing is recorded")
	}
	if usesAppAuth(cr(map[string]string{"token": "ghp_x"})) {
		t.Error("a token alone was read as App auth")
	}
}

// An App config that cannot possibly work, beside a token that can, means the
// token. Choosing the App there fails about a setup the operator abandoned
// while a working credential sits unused next to it.
func TestAnUnusableAppConfigYieldsToAWorkingToken(t *testing.T) {
	abandoned := cr(map[string]string{"app_id": "4736085", "token": "ghp_works"})
	if usesAppAuth(abandoned) {
		t.Error("an incomplete App config was preferred over a usable token")
	}

	// Complete App config still wins, token or not.
	complete := cr(map[string]string{"app_id": "1", "app_private_key": "KEY", "token": "ghp_old"})
	if !usesAppAuth(complete) {
		t.Error("a complete App config was ignored")
	}

	// Incomplete App and NO token: still App, so the error names the field
	// they actually need rather than "no token configured".
	stuck := cr(map[string]string{"app_id": "1"})
	if !usesAppAuth(stuck) {
		t.Error("with nothing to fall back on, the App path should still be taken")
	}
}

// GitHub's downloads folder also holds the App's PUBLIC key, and often an
// unrelated .pem. Uploading the wrong one otherwise surfaces at the first API
// call as an authentication error, sending the operator to look at permissions.
func TestTheWrongFileIsRefusedAtUploadTime(t *testing.T) {
	c := New("")

	cases := map[string]struct{ key, want string }{
		"the public key": {
			"-----BEGIN PUBLIC KEY-----\nMIIB\n-----END PUBLIC KEY-----", "PUBLIC key",
		},
		"not a key at all": {
			"just some text someone pasted", "does not look like a PEM",
		},
		"a truncated pem": {
			"-----BEGIN RSA PRIVATE KEY-----\nnot-base64!!\n-----END RSA PRIVATE KEY-----",
			"could not be read",
		},
	}
	for name, tc := range cases {
		err := c.ValidateCredentials(cr(map[string]string{
			"auth_method": "app", "app_id": "1", "app_private_key": tc.key,
		}))
		if err == nil {
			t.Errorf("%s was accepted", name)
			continue
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: message does not say what is wrong: %v", name, err)
		}
	}
}

// An App ID is the number on the App's page, and people paste the name.
func TestANamedAppIDIsRefused(t *testing.T) {
	c := New("")
	err := c.ValidateCredentials(cr(map[string]string{
		"auth_method": "app", "app_id": "my-karmax-bot", "app_private_key": validTestKey(t),
	}))
	if err == nil || !strings.Contains(err.Error(), "number") {
		t.Errorf("a named App ID was accepted: %v", err)
	}
}

// A real key must pass, or the check is just an obstacle.
func TestAValidKeyIsAccepted(t *testing.T) {
	c := New("")
	if err := c.ValidateCredentials(cr(map[string]string{
		"auth_method": "app", "app_id": "1234567", "app_private_key": validTestKey(t),
	})); err != nil {
		t.Errorf("a valid App setup was refused: %v", err)
	}
}

// The pre-2021 40-hex format was revoked wholesale, so it will be rejected by
// GitHub — saying so now beats a puzzling 401 later.
func TestAnOldStyleTokenIsFlagged(t *testing.T) {
	c := New("")
	err := c.ValidateCredentials(cr(map[string]string{
		"auth_method": "pat", "token": strings.Repeat("a1b2", 10), // 40 hex chars
	}))
	if err == nil || !strings.Contains(err.Error(), "old-style") {
		t.Errorf("an old-format token was not flagged: %v", err)
	}
	// A modern one passes.
	if err := c.ValidateCredentials(cr(map[string]string{
		"auth_method": "pat", "token": "ghp_abcdefghijklmnopqrstuvwxyz0123456789",
	})); err != nil {
		t.Errorf("a modern token was refused: %v", err)
	}
}

func validTestKey(t *testing.T) string {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{
		Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key),
	}))
}
