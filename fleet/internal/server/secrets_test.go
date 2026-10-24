package server

import (
	"encoding/base64"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/MelloB1989/karmax/fleet/internal/daemon"
	"github.com/MelloB1989/karmax/fleet/internal/ledger"
)

const fakeKey = "sk-test-0123456789abcdef"

func (f *fake) SecretPut(name string, value []byte, _ time.Time) error {
	if name == "bad name" {
		return fmt.Errorf("%w: secret name %q", daemon.ErrBadInput, name)
	}
	f.calls = append(f.calls, fmt.Sprintf("put %s %d", name, len(value)))
	return nil
}
func (f *fake) Secrets(time.Time) ([]ledger.SecretInfo, error) {
	return []ledger.SecretInfo{{Name: "openai", Grants: []ledger.Grant{{Task: "T1"}}}}, nil
}
func (f *fake) SecretGrant(name, task, agent, by string, ttl time.Duration, _ time.Time) error {
	f.calls = append(f.calls, fmt.Sprintf("grant %s %s %s %s %s", name, task, agent, by, ttl))
	return nil
}
func (f *fake) SecretRevoke(name, task, agent string) error {
	if task == "T9" {
		return fmt.Errorf("%w: %s is not granted to T9", daemon.ErrNoGrant, name)
	}
	f.calls = append(f.calls, fmt.Sprintf("revoke %s %s %s", name, task, agent))
	return nil
}
func (f *fake) SecretRemove(name string) error {
	if name == "gone" {
		return fmt.Errorf("%w: gone", ledger.ErrNoSecret)
	}
	f.calls = append(f.calls, "rm "+name)
	return nil
}
func (f *fake) SecretRead(agent, name string, _ time.Time) ([]byte, error) {
	f.calls = append(f.calls, "read "+agent+" "+name)
	return []byte(fakeKey), nil
}

func putBody(name string) string {
	return fmt.Sprintf(`{"name":%q,"value":%q}`, name, base64.StdEncoding.EncodeToString([]byte(fakeKey)))
}

// The full scope keeps and grants secrets; nobody else touches them, and an
// agent reads one only as the agent its identity proves.
func TestSecretRoutesAreTheFullScopesAndAnAgentReadsOnlyAsAVerifiedAgent(t *testing.T) {
	f, h := setup(t)
	routes := []struct{ method, path, body string }{
		{"POST", "/v1/secrets", putBody("openai")},
		{"GET", "/v1/secrets", ""},
		{"POST", "/v1/secrets/openai/grant", `{"task":"T1"}`},
		{"POST", "/v1/secrets/openai/revoke", `{"task":"T1"}`},
		{"DELETE", "/v1/secrets/openai", ""},
	}
	for _, r := range routes {
		for _, tok := range []string{"relay", "console"} {
			if rec := do(t, h, r.method, r.path, tok, r.body); rec.Code != 403 {
				t.Errorf("%s %s as %s: %d", r.method, r.path, tok, rec.Code)
			}
		}
		if rec := do(t, h, r.method, r.path, "full", r.body); rec.Code != 200 {
			t.Errorf("%s %s as full: %d %s", r.method, r.path, rec.Code, rec.Body)
		}
	}
	// A relay token alone is no agent: the name an agent claims is not asked.
	for _, tok := range []string{"relay", "full", "console"} {
		rec := do(t, h, "POST", "/v1/secrets/openai/read", tok, `{"agent":"agent-03"}`)
		if rec.Code != 403 || strings.Contains(rec.Body.String(), fakeKey) {
			t.Errorf("read as %s: %d %s", tok, rec.Code, rec.Body)
		}
	}
	for _, c := range f.calls {
		if strings.HasPrefix(c, "read ") {
			t.Fatalf("a read reached the fleet without a verified agent: %v", f.calls)
		}
	}
}

func TestAMemberMayNotEvenListSecrets(t *testing.T) {
	f, h, _ := tailnetSetup(t)
	for _, r := range []struct{ method, path, body string }{
		{"GET", "/v1/secrets", ""},
		{"POST", "/v1/secrets", putBody("openai")},
		{"POST", "/v1/secrets/openai/grant", `{"agent":"agent-03"}`},
		{"POST", "/v1/secrets/openai/read", ""},
		{"DELETE", "/v1/secrets/openai", ""},
	} {
		if rec := tdo(t, h, r.method, r.path, ramIP, "", r.body); rec.Code != 403 {
			t.Errorf("member %s %s: %d", r.method, r.path, rec.Code)
		}
	}
	if len(f.calls) != 0 {
		t.Fatalf("a member's call reached the fleet: %v", f.calls)
	}
	// An operator on the tailnet has the full scope, and grants as themselves.
	if rec := tdo(t, h, "POST", "/v1/secrets/openai/grant", opIP, "", `{"agent":"agent-03","ttl":"2h"}`); rec.Code != 200 {
		t.Fatalf("operator grant: %d %s", rec.Code, rec.Body)
	}
	if want := "grant openai  agent-03 op@example.com 2h0m0s"; f.calls[0] != want {
		t.Fatalf("calls = %q, want %q", f.calls, want)
	}
}

func TestSecretRoutesPassWhatTheyAreGivenAndNeverAValueBack(t *testing.T) {
	f, h := setup(t)
	do(t, h, "POST", "/v1/secrets", "full", putBody("openai"))
	do(t, h, "POST", "/v1/secrets/openai/grant", "full", `{"task":"T1","ttl":"90m"}`)
	do(t, h, "POST", "/v1/secrets/openai/revoke", "full", `{"agent":"agent-03"}`)
	do(t, h, "DELETE", "/v1/secrets/openai", "full", "")
	want := fmt.Sprintf("put openai %d|grant openai T1  operator 1h30m0s|revoke openai  agent-03|rm openai", len(fakeKey))
	if got := strings.Join(f.calls, "|"); got != want {
		t.Fatalf("calls = %s\nwant    %s", got, want)
	}
	if rec := do(t, h, "GET", "/v1/secrets", "full", ""); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"openai"`) {
		t.Fatalf("ls: %d %s", rec.Code, rec.Body)
	}
	for _, c := range []struct {
		method, path, body string
		want               int
	}{
		{"POST", "/v1/secrets/openai/grant", `{"task":"T1","ttl":"soon"}`, 400},
		{"POST", "/v1/secrets/openai/revoke", `{"task":"T9"}`, 404},
		{"DELETE", "/v1/secrets/gone", "", 404},
		{"POST", "/v1/secrets", putBody("bad name"), 400},
		// A body that does not parse is not quoted back.
		{"POST", "/v1/secrets", `{"name":"openai","value":"` + fakeKey + `!"}`, 400},
		{"POST", "/v1/secrets", `{"name":"openai","value":` + fakeKey + `}`, 400},
	} {
		rec := do(t, h, c.method, c.path, "full", c.body)
		if rec.Code != c.want {
			t.Errorf("%s %s %s: %d, want %d", c.method, c.path, c.body, rec.Code, c.want)
		}
		if strings.Contains(rec.Body.String(), fakeKey) || strings.Contains(rec.Body.String(), "sk-test") {
			t.Errorf("%s %s: the answer quotes the value: %s", c.method, c.path, rec.Body)
		}
	}
}
