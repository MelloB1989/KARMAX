package aws

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const sampleConfig = `
[default]
login_session = arn:aws:iam::111122223333:user/example
region = eu-west-1

[profile ops-admin]
sso_session = example-sso
sso_account_id = 111122223333
sso_role_name = AdministratorAccess
region = us-east-1

[sso-session example-sso]
sso_start_url = https://example.awsapps.com/start
sso_region = us-east-1

[sso-session default]
sso_start_url = https://other.awsapps.com/start
region = ap-northeast-1

[profile ops-readonly]
sso_session = example-sso
sso_account_id = 111122223333
sso_role_name = ReadOnlyAccess
region = us-east-1

[profile legacy]
sso_start_url = https://legacy.awsapps.com/start
sso_account_id = 444455556666
sso_role_name = ViewOnly
region = ap-south-1

[profile static]
region = us-west-2

[profile assumed]
role_arn = arn:aws:iam::777788889999:role/Reader
source_profile = static

[profile noregion]
sso_session = example-sso
sso_account_id = 999900001111
`

// The credentials file holds keys; only its section names are read, so a
// profile that lives there is known to exist without a key ever entering memory.
const sampleCredentials = `
[static]
aws_access_key_id = example-key-id-zero
aws_secret_access_key = not-a-real-secret

[keys-only]
aws_access_key_id = example-key-id-one
aws_secret_access_key = not-a-real-secret-either
`

func writeAWSFiles(t *testing.T) (string, string) {
	t.Helper()
	dir := t.TempDir()
	cfg, creds := filepath.Join(dir, "config"), filepath.Join(dir, "credentials")
	if err := os.WriteFile(cfg, []byte(sampleConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(creds, []byte(sampleCredentials), 0o600); err != nil {
		t.Fatal(err)
	}
	return cfg, creds
}

func TestProfilesAreReadFromTheSharedConfigInOrder(t *testing.T) {
	cfg, creds := writeAWSFiles(t)
	ps, err := LoadProfiles(cfg, creds)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, p := range ps {
		names = append(names, p.Name)
	}
	want := []string{"default", "ops-admin", "ops-readonly", "legacy", "static", "assumed", "noregion", "keys-only"}
	if !reflect.DeepEqual(names, want) {
		t.Fatalf("profiles = %v, want %v", names, want)
	}
	byName := map[string]Profile{}
	for _, p := range ps {
		byName[p.Name] = p
	}
	if p := byName["ops-admin"]; p.AccountID != "111122223333" || p.Region != "us-east-1" || p.SSOSession != "example-sso" {
		t.Errorf("ops-admin = %+v", p)
	}
	if p := byName["assumed"]; p.AccountID != "777788889999" {
		t.Errorf("an assumed role's account comes from its ARN: %+v", p)
	}
	if p := byName["assumed"]; p.Region != "eu-west-1" {
		t.Errorf("a profile without a region takes the default profile's: %q", p.Region)
	}
	if p := byName["static"]; !p.Static || p.AccountID != "" {
		t.Errorf("static = %+v; its account is learned by asking, not guessed", p)
	}
	if p := byName["keys-only"]; !p.Static || p.Region != "eu-west-1" {
		t.Errorf("a profile only in the credentials file = %+v", p)
	}
	if p := byName["default"]; !p.LoginSession {
		t.Errorf("default = %+v, want an aws login profile", p)
	}
}

// A secret in the credentials file must never be held, so it can never be
// logged or reported by accident.
func TestNoCredentialValueIsKept(t *testing.T) {
	cfg, creds := writeAWSFiles(t)
	ps, err := LoadProfiles(cfg, creds)
	if err != nil {
		t.Fatal(err)
	}
	dump := strings.ToLower(strings.Join(func() []string {
		var s []string
		for _, p := range ps {
			s = append(s, p.Name, p.Region, p.AccountID, p.SSOSession, p.SSOStartURL, p.SourceProfile)
		}
		return s
	}(), " "))
	for _, secret := range []string{"example-key-id", "not-a-real-secret"} {
		if strings.Contains(dump, secret) {
			t.Fatalf("a credential value was kept: %q", secret)
		}
	}
}

func TestAMissingConfigFileIsNoProfilesNotAnError(t *testing.T) {
	ps, err := LoadProfiles(filepath.Join(t.TempDir(), "nope"), filepath.Join(t.TempDir(), "nope"))
	if err != nil || len(ps) != 0 {
		t.Fatalf("got %v, %v", ps, err)
	}
}

// Each kind of login has its own way back in, and health says exactly which.
func TestEachProfileKindNamesItsOwnLoginFix(t *testing.T) {
	cfg, creds := writeAWSFiles(t)
	ps, _ := LoadProfiles(cfg, creds)
	got := map[string]string{}
	for _, p := range ps {
		got[p.Name] = p.LoginFix()
	}
	for name, want := range map[string]string{
		"ops-admin": "aws sso login --sso-session example-sso",
		"legacy":    "aws sso login --profile legacy",
		"default":   "aws login",
		"static":    "aws configure --profile static",
		"assumed":   "aws configure --profile static",
	} {
		if !strings.Contains(got[name], want) {
			t.Errorf("%s fix = %q, want it to contain %q", name, got[name], want)
		}
	}
}

func TestAnAllowListKeepsOnlyNamedProfilesInItsOrder(t *testing.T) {
	cfg, creds := writeAWSFiles(t)
	ps, _ := LoadProfiles(cfg, creds)
	got := Allowed(ps, []string{"static", "ops-readonly", "not-there"})
	if len(got) != 2 || got[0].Name != "static" || got[1].Name != "ops-readonly" {
		t.Fatalf("allowed = %+v", got)
	}
	if all := Allowed(ps, nil); len(all) != len(ps) {
		t.Fatalf("an empty allow-list keeps every profile, got %d of %d", len(all), len(ps))
	}
}
