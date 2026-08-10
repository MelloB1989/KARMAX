package integration

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/MelloB1989/karmax/internal/store"
	"github.com/MelloB1989/karmax/pkg/connectorkit"
	"go.uber.org/zap"
)

func testRegistry(t *testing.T, config ConfigLookup) (*Registry, *store.Store) {
	t.Helper()
	db, err := store.New(filepath.Join(t.TempDir(), "k.db"), zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return NewRegistry(NewResolver(db, config)), db
}

// A key can come from a file OR a login, and the login wins.
//
// Both have to work: a container install wants the key in karmax.yaml, a laptop
// wants `karmax login`. The login wins because it is the more recent deliberate
// act — a yaml key is a setting somebody wrote once, and silently overriding
// what they just typed is the wrong way round.
func TestAStoredCredentialWinsOverTheConfigFile(t *testing.T) {
	reg, db := testRegistry(t, func(string) map[string]string {
		return map[string]string{"token": "from-the-file"}
	})
	reg.Register(APIKey(Manifest{ID: "slack", Name: "Slack",
		Config: []connectorkit.ConfigField{{Key: "token", Required: true}}}, "token", nil))

	creds, source, err := reg.Credentials("slack")
	if err != nil {
		t.Fatal(err)
	}
	if creds.Get("token") != "from-the-file" || source != SourceConfig {
		t.Fatalf("with nothing stored it should read the file; got %q from %q", creds.Get("token"), source)
	}

	if err := db.SaveCredential(store.Credential{
		Connector: "slack", Config: map[string]string{"token": "from-the-login"}, Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}
	creds, source, err = reg.Credentials("slack")
	if err != nil {
		t.Fatal(err)
	}
	if creds.Get("token") != "from-the-login" || source != SourceStore {
		t.Errorf("a login should win; got %q from %q", creds.Get("token"), source)
	}
}

// Values merge rather than one source replacing the other wholesale, so a
// client id in the yaml survives a login that only obtains a token.
func TestSourcesMergePerValue(t *testing.T) {
	reg, db := testRegistry(t, func(string) map[string]string {
		return map[string]string{"client_id": "from-the-file"}
	})
	reg.Register(APIKey(Manifest{ID: "notion", Config: []connectorkit.ConfigField{
		{Key: "client_id"}, {Key: "api_key"},
	}}, "api_key", nil))

	if err := db.SaveCredential(store.Credential{
		Connector: "notion", Config: map[string]string{"api_key": "from-the-login"}, Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}
	creds, _, err := reg.Credentials("notion")
	if err != nil {
		t.Fatal(err)
	}
	if creds.Get("client_id") != "from-the-file" {
		t.Error("logging in erased a value that only the config file had")
	}
	if creds.Get("api_key") != "from-the-login" {
		t.Error("the stored value did not come through")
	}
}

// The environment is the last resort, under both.
func TestTheEnvironmentIsTheFallback(t *testing.T) {
	t.Setenv(EnvName("telegram", "token"), "from-the-env")
	reg, _ := testRegistry(t, nil)
	reg.Register(APIKey(Manifest{ID: "telegram", Config: []connectorkit.ConfigField{
		{Key: "token", Required: true},
	}}, "token", nil))

	creds, source, err := reg.Credentials("telegram")
	if err != nil {
		t.Fatal(err)
	}
	if creds.Get("token") != "from-the-env" || source != SourceEnv {
		t.Errorf("got %q from %q", creds.Get("token"), source)
	}
}

// Nothing stored is "not connected", not a database error.
//
// store.Credential returned sql.ErrNoRows for an integration nobody had logged
// into, which is every integration on a fresh install — so they all reported a
// database failure, and a REAL read failure was indistinguishable among them.
func TestAnUnconfiguredIntegrationIsNotAnError(t *testing.T) {
	reg, _ := testRegistry(t, nil)
	reg.Register(APIKey(Manifest{ID: "slack", Config: []connectorkit.ConfigField{
		{Key: "token", Required: true},
	}}, "token", nil))

	creds, source, err := reg.Credentials("slack")
	if err != nil {
		t.Fatalf("an unconfigured integration reported an error: %v", err)
	}
	if creds.Get("token") != "" || source != SourceNone {
		t.Errorf("got %q from %q", creds.Get("token"), source)
	}

	st := reg.Check(context.Background(), "slack")
	if st.Configured {
		t.Error("reported as configured with nothing set")
	}
	if st.Error != "" {
		t.Errorf("reported an error rather than simply being unconfigured: %q", st.Error)
	}
}

// Health is a real call, so a present-but-dead key is not "working".
func TestAPresentButDeadCredentialIsNotHealthy(t *testing.T) {
	reg, _ := testRegistry(t, func(string) map[string]string {
		return map[string]string{"token": "expired"}
	})
	reg.Register(APIKey(Manifest{ID: "slack", Config: []connectorkit.ConfigField{{Key: "token"}}},
		"token", func(context.Context, connectorkit.Credentials) error {
			return errors.New("invalid_auth")
		}))

	st := reg.Check(context.Background(), "slack")
	if !st.Configured {
		t.Error("a key IS set, so it is configured")
	}
	if st.Healthy {
		t.Error("a key the provider refuses is not healthy")
	}
	if st.Error == "" {
		t.Error("the reason was not recorded")
	}
}

// For a CLI session KARMAX holds nothing, so being signed in IS being
// configured — anything else reports a working WhatsApp as not connected.
func TestACLISessionIsConfiguredWhenItIsSignedIn(t *testing.T) {
	signedIn := true
	reg, _ := testRegistry(t, nil)
	reg.Register(CLISession(Manifest{ID: "whatsapp"}, "wacli",
		func(context.Context, connectorkit.Credentials) error {
			if signedIn {
				return nil
			}
			return errors.New("not paired")
		}))

	if st := reg.Check(context.Background(), "whatsapp"); !st.Configured || !st.Healthy {
		t.Errorf("a signed-in CLI session reported configured=%v healthy=%v", st.Configured, st.Healthy)
	}
	signedIn = false
	if st := reg.Check(context.Background(), "whatsapp"); st.Configured || st.Healthy {
		t.Errorf("a signed-out CLI session reported configured=%v healthy=%v", st.Configured, st.Healthy)
	}
}

// Logging in does not save a credential that does not work.
//
// The point is to fail while the operator still has the provider's page open,
// rather than hours later inside a loop that cannot explain itself.
func TestLoginRefusesToSaveACredentialThatFails(t *testing.T) {
	reg, db := testRegistry(t, nil)
	reg.Register(APIKey(Manifest{ID: "slack", Name: "Slack",
		Config: []connectorkit.ConfigField{{Key: "token", Required: true, Secret: true}}},
		"token", func(context.Context, connectorkit.Credentials) error {
			return errors.New("invalid_auth")
		}))

	err := reg.Login(context.Background(), "slack", &scriptedPrompter{answers: []string{"bad-token"}})
	if err == nil {
		t.Fatal("a credential the provider refused was accepted")
	}
	cred, _ := db.Credential("slack")
	if cred != nil {
		t.Error("it was saved anyway")
	}
}

// A working credential is saved, and shows as coming from the login.
func TestLoginSavesAWorkingCredential(t *testing.T) {
	reg, db := testRegistry(t, nil)
	reg.Register(APIKey(Manifest{ID: "slack", Name: "Slack",
		Config: []connectorkit.ConfigField{{Key: "token", Required: true, Secret: true}}},
		"token", func(_ context.Context, c connectorkit.Credentials) error {
			if c.Get("token") == "good-token" {
				return nil
			}
			return errors.New("invalid_auth")
		}))

	p := &scriptedPrompter{answers: []string{"good-token"}}
	if err := reg.Login(context.Background(), "slack", p); err != nil {
		t.Fatalf("login: %v", err)
	}
	cred, err := db.Credential("slack")
	if err != nil || cred == nil {
		t.Fatalf("nothing was saved: %v", err)
	}
	if cred.Config["token"] != "good-token" {
		t.Errorf("saved %q", cred.Config["token"])
	}
	// A secret must never be echoed back at the operator.
	for _, said := range p.said {
		if said == "good-token" {
			t.Error("the token was printed to the terminal")
		}
	}
}

// Forgetting hands control back to the config file rather than disconnecting.
func TestForgettingFallsBackToTheFile(t *testing.T) {
	reg, db := testRegistry(t, func(string) map[string]string {
		return map[string]string{"token": "from-the-file"}
	})
	reg.Register(APIKey(Manifest{ID: "slack", Config: []connectorkit.ConfigField{{Key: "token"}}}, "token", nil))

	if err := db.SaveCredential(store.Credential{
		Connector: "slack", Config: map[string]string{"token": "from-the-login"}, Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}
	if err := reg.Forget("slack"); err != nil {
		t.Fatal(err)
	}
	creds, source, _ := reg.Credentials("slack")
	if creds.Get("token") != "from-the-file" || source != SourceConfig {
		t.Errorf("after forgetting, got %q from %q", creds.Get("token"), source)
	}
}

// The env variable name is derived, so an integration does not have to
// document one.
func TestEnvNameIsPredictable(t *testing.T) {
	for _, tc := range []struct{ id, field, want string }{
		{"slack", "bot_token", "KARMAX_SLACK_BOT_TOKEN"},
		{"whatsapp-main", "token", "KARMAX_WHATSAPP_MAIN_TOKEN"},
		{"github:work", "api_key", "KARMAX_GITHUB_WORK_API_KEY"},
	} {
		if got := EnvName(tc.id, tc.field); got != tc.want {
			t.Errorf("EnvName(%q,%q) = %q, want %q", tc.id, tc.field, got, tc.want)
		}
	}
}

// An account makes a second login to the same provider a separate identity.
func TestAccountsAreSeparateIdentities(t *testing.T) {
	if provider, account := SplitID("github:work"); provider != "github" || account != "work" {
		t.Errorf("SplitID = %q, %q", provider, account)
	}
	if provider, account := SplitID("github"); provider != "github" || account != "" {
		t.Errorf("SplitID = %q, %q", provider, account)
	}
}

// scriptedPrompter answers in order, for testing a flow without a terminal.
type scriptedPrompter struct {
	answers []string
	at      int
	said    []string
}

func (p *scriptedPrompter) Ask(connectorkit.ConfigField) (string, error) {
	if p.at >= len(p.answers) {
		return "", errors.New("no more scripted answers")
	}
	p.at++
	return p.answers[p.at-1], nil
}

func (p *scriptedPrompter) Say(format string, args ...any) {
	p.said = append(p.said, fmt.Sprintf(format, args...))
}

func (p *scriptedPrompter) Open(string) error { return nil }
