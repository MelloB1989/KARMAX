package googleacct

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/MelloB1989/karmax/internal/sources/gcal"
)

const gogList = `{"accounts":[
 {"email":"Priya@Example.com","services":["calendar","chat","gmail"],"auth":"oauth"},
 {"email":"ops@example.org","services":["service-account"],"auth":"service_account"}
]}`

func gwsStatus(user string, refresh bool) string {
	r := "false"
	if refresh {
		r = "true"
	}
	return "Using keyring backend: file\n" + `{"auth_method":"oauth2","has_refresh_token":` + r +
		`,"token_valid":true,"scopes":["email","https://www.googleapis.com/auth/calendar","https://www.googleapis.com/auth/chat.messages","https://www.googleapis.com/auth/chat.spaces"],"user":"` + user + `"}`
}

func TestDiscoveryMergesBothCLIsAndRanksEachAccountsLogins(t *testing.T) {
	f := &fakeCLI{}
	f.on("gog", "", "auth list", reply{out: gogList})
	f.on("gws", "/cfg/gws", "auth status", reply{out: gwsStatus("priya@example.com", true)})
	f.on("gws", "/cfg/gws-side", "auth status", reply{out: gwsStatus("dev@example.net", false)})
	h := hostWith(f, "/cfg/gws", "/cfg/gws-side")

	d := h.Discover(context.Background())
	if len(d.Errors) != 0 {
		t.Fatalf("discovery errors: %v", d.Errors)
	}
	got := map[string]Account{}
	for _, a := range d.Accounts {
		got[a.Email] = a
	}
	if len(got) != 3 {
		t.Fatalf("accounts %v, want three distinct addresses", d.Accounts)
	}
	p := got["priya@example.com"]
	if len(p.Logins) != 2 || p.Logins[0].Path != Gws || p.Logins[1].Path != Gog {
		t.Fatalf("priya's logins %+v: want gws (full API) before gog OAuth", p.Logins)
	}
	if p.Logins[0].Dir != "/cfg/gws" {
		t.Errorf("gws login dir %q", p.Logins[0].Dir)
	}
	if ops := got["ops@example.org"]; len(ops.Logins) != 1 || !ops.Logins[0].ServiceAccount {
		t.Errorf("a gog service account should be known as one: %+v", ops)
	}
	dev := got["dev@example.net"]
	if dev.Logins[0].Problem == "" {
		t.Fatal("a gws login without a refresh token was taken as usable")
	}
	if fix := dev.Logins[0].Fix(); !strings.Contains(fix, "GOOGLE_WORKSPACE_CLI_CONFIG_DIR=/cfg/gws-side") || !strings.Contains(fix, "gws auth login") {
		t.Errorf("fix for a side gws dir: %q", fix)
	}
	if fix := p.Logins[1].Fix(); fix != "gog auth add priya@example.com --services calendar,chat,gmail --force-consent" {
		t.Errorf("gog fix: %q", fix)
	}
}

// A service account never needs a person to sign in again, which makes it the
// most robust way to reach an account.
func TestAServiceAccountOutranksEverything(t *testing.T) {
	f := &fakeCLI{}
	f.on("gog", "", "auth list", reply{out: `{"accounts":[{"email":"priya@example.com","services":["service-account"],"auth":"service_account"}]}`})
	f.on("gws", "", "auth status", reply{out: gwsStatus("priya@example.com", true)})
	d := hostWith(f, "/cfg/gws").Discover(context.Background())
	if l := d.Accounts[0].Logins; l[0].Path != Gog || !l[0].ServiceAccount {
		t.Fatalf("logins %+v", l)
	}
}

func TestDiscoveryCarriesOnWhenOneCLIFails(t *testing.T) {
	f := &fakeCLI{}
	f.on("gog", "", "auth list", reply{errOut: "keyring locked", code: 10})
	f.on("gws", "", "auth status", reply{out: gwsStatus("priya@example.com", true)})
	d := hostWith(f, "/cfg/gws").Discover(context.Background())
	if len(d.Accounts) != 1 || len(d.Errors) != 1 || !strings.Contains(d.Errors[0], "keyring locked") {
		t.Fatalf("accounts %v errors %v", d.Accounts, d.Errors)
	}
}

// gws asks Google whether its token is valid, which takes seconds; a login
// only changes when its stored credentials do, so it is asked again only then
// (or when a call has found the login signed out).
func TestAGwsLoginIsAskedAgainOnlyWhenItsCredentialsChange(t *testing.T) {
	f := &fakeCLI{}
	f.on("gog", "", "auth list", reply{out: `{"accounts":[]}`})
	f.on("gws", "", "auth status", reply{out: gwsStatus("priya@example.com", true)})
	h := hostWith(f, "/cfg/gws")
	stamp := "v1"
	h.Stamp = func(string) string { return stamp }
	statuses := func() int {
		n := 0
		for _, c := range f.calls {
			if c.bin == "gws" && strings.Contains(strings.Join(c.args, " "), "auth status") {
				n++
			}
		}
		return n
	}
	for i := 0; i < 3; i++ {
		if d := h.Discover(context.Background()); len(d.Accounts) != 1 {
			t.Fatalf("discovery %d: %v", i, d.Accounts)
		}
	}
	if n := statuses(); n != 1 {
		t.Fatalf("gws auth status ran %d times for unchanged credentials", n)
	}
	stamp = "v2"
	h.Discover(context.Background())
	if n := statuses(); n != 2 {
		t.Fatalf("changed credentials were not looked at again (%d runs)", n)
	}
	h.Forget()
	h.Discover(context.Background())
	if n := statuses(); n != 3 {
		t.Fatalf("after a sign-in failure the login must be looked at again (%d runs)", n)
	}
}

func TestASignedOutGwsDirIsNotAnAccount(t *testing.T) {
	f := &fakeCLI{}
	f.on("gog", "", "auth list", reply{out: `{"accounts":[]}`})
	h := hostWith(f, "/cfg/gws-empty")
	h.HasLogin = func(string) bool { return false }
	if d := h.Discover(context.Background()); len(d.Accounts) != 0 {
		t.Fatalf("a gws dir with no stored login became %v", d.Accounts)
	}
}

const calPage = `{"items":[{"id":"e1","status":"confirmed","summary":"Design review","htmlLink":"https://calendar.example.test/e1",
 "start":{"dateTime":"2026-11-03T10:00:00+05:30"},"end":{"dateTime":"2026-11-03T10:30:00+05:30"},
 "created":"2026-10-30T08:00:00Z","updated":"2026-10-31T08:00:00.123Z","organizer":{"email":"rahul@example.com"},
 "attendees":[{"email":"rahul@example.com","organizer":true,"responseStatus":"accepted"},{"email":"priya@example.com","self":true,"responseStatus":"needsAction"}],
 "hangoutLink":"https://meet.example.test/abc"}],"nextSyncToken":"sync-1"}`

// When the preferred login has lost its sign-in, the next one that reaches
// the same account is used, so one revoked CLI does not stop the source.
func TestAnAuthFailureFallsBackToTheNextLogin(t *testing.T) {
	f := &fakeCLI{}
	f.on("gog", "", "auth list", reply{out: `{"accounts":[{"email":"priya@example.com","services":["calendar","chat"],"auth":"oauth"}]}`})
	f.on("gws", "", "auth status", reply{out: gwsStatus("priya@example.com", true)})
	f.on("gws", "", "calendar events list", reply{errOut: `{"error":{"code":401,"message":"invalid_grant"}}`, code: 2})
	f.on("gog", "", "calendar events", reply{out: `{"events":[{"id":"e2","status":"confirmed","summary":"Standup","start":{"dateTime":"2026-11-03T09:00:00Z"},"end":{"dateTime":"2026-11-03T09:15:00Z"},"updated":"2026-10-31T08:00:00Z"}]}`})
	c := NewClient(hostWith(f, "/cfg/gws"), time.UTC)

	page, err := c.Events(context.Background(), "priya@example.com", gcal.Query{From: time.Now(), To: time.Now().Add(time.Hour)})
	if err != nil {
		t.Fatalf("events: %v", err)
	}
	if len(page.Events) != 1 || page.Events[0].ID != "e2" || !page.Complete {
		t.Fatalf("page %+v", page)
	}
	if got := f.find(t, "gog", "calendar", "events"); argValue(got.args, "--account") != "priya@example.com" {
		t.Errorf("gog was not pointed at the account: %v", got)
	}
	h := c.Health("priya@example.com")
	if h.Path != Gog || h.Err != nil {
		t.Errorf("health after a fallback: %+v", h)
	}
}

func TestWhenEveryLoginFailsTheErrorSaysWhatToRun(t *testing.T) {
	f := &fakeCLI{}
	f.on("gog", "", "auth list", reply{out: `{"accounts":[]}`})
	f.on("gws", "", "auth status", reply{out: gwsStatus("priya@example.com", true)})
	f.on("gws", "", "calendar events list", reply{errOut: `Error: invalid_grant: Token has been expired or revoked.`, code: 2})
	c := NewClient(hostWith(f, "/cfg/gws"), time.UTC)
	_, err := c.Events(context.Background(), "priya@example.com", gcal.Query{})
	var ae *AuthError
	if !errors.As(err, &ae) {
		t.Fatalf("want an AuthError, got %v", err)
	}
	if !strings.Contains(err.Error(), "gws auth login") || !strings.Contains(err.Error(), "gog auth add priya@example.com") {
		t.Errorf("the error should give the commands that fix it: %v", err)
	}
	if c.Health("priya@example.com").Err == nil {
		t.Error("health does not show the failure")
	}
}

func TestAnUnknownAccountIsRefusedByName(t *testing.T) {
	f := &fakeCLI{}
	f.on("gog", "", "auth list", reply{out: `{"accounts":[]}`})
	f.on("gws", "", "auth status", reply{out: gwsStatus("priya@example.com", true)})
	c := NewClient(hostWith(f, "/cfg/gws"), time.UTC)
	_, err := c.Events(context.Background(), "someone@example.com", gcal.Query{})
	if err == nil || !strings.Contains(err.Error(), "priya@example.com") {
		t.Fatalf("an unknown account should list the known ones: %v", err)
	}
}

func TestTokensNeverReachAnErrorMessage(t *testing.T) {
	f := &fakeCLI{}
	f.on("gog", "", "auth list", reply{out: `{"accounts":[]}`})
	f.on("gws", "", "auth status", reply{out: gwsStatus("priya@example.com", true)})
	f.on("gws", "", "calendar events list", reply{errOut: `boom with ya29.a0AfB_secretSECRET and 1//0gSECRETrefresh`, code: 1})
	c := NewClient(hostWith(f, "/cfg/gws"), time.UTC)
	_, err := c.Events(context.Background(), "priya@example.com", gcal.Query{})
	if err == nil || strings.Contains(err.Error(), "SECRET") {
		t.Fatalf("error leaked a token: %v", err)
	}
}

// The incident: the host's DNS failed for hours. gws could not fetch a token
// and said so as an auth error, and KARMAX paged that three healthy accounts
// were signed out. Not reaching Google says nothing about the sign-in.
func TestANetworkFailureIsNotASignOut(t *testing.T) {
	for _, detail := range []string{
		`error[auth]: Authentication failed: Failed to get token: Connection failure: Hyper error: client error (Connect): tcp connect error: Connection timed out (os error 110) { "error": { "code": 401, "message": "Authentication failed: Failed to get token: Connection failure", "reason": "authError" } }`,
		`error[auth]: Authentication failed: Failed to get token: Server error: internal_failure: internal_failure { "error": { "code": 401, "reason": "authError" } }`,
	} {
		f := &fakeCLI{}
		f.on("gog", "", "auth list", reply{out: `{"accounts":[]}`})
		f.on("gws", "", "auth status", reply{out: gwsStatus("priya@example.com", true)})
		f.on("gws", "", "calendar events list", reply{errOut: detail, code: 2})
		c := NewClient(hostWith(f, "/cfg/gws"), time.UTC)
		_, err := c.Events(context.Background(), "priya@example.com", gcal.Query{})
		var ae *AuthError
		if err == nil || errors.As(err, &ae) {
			t.Errorf("a failure to reach Google was read as a sign-out: %v", err)
		}
	}
	// Google refusing the login is still one, whatever else the text says.
	f := &fakeCLI{}
	f.on("gog", "", "auth list", reply{out: `{"accounts":[]}`})
	f.on("gws", "", "auth status", reply{out: gwsStatus("priya@example.com", true)})
	f.on("gws", "", "calendar events list", reply{errOut: `error[auth]: Failed to get token: Server error: invalid_grant: Token has been expired or revoked.`, code: 2})
	_, err := NewClient(hostWith(f, "/cfg/gws"), time.UTC).Events(context.Background(), "priya@example.com", gcal.Query{})
	var ae *AuthError
	if !errors.As(err, &ae) {
		t.Errorf("a revoked login must still be reported as signed out: %v", err)
	}
}

// gws checks its token with Google when asked who is signed in, so with the
// network down it cannot answer. The stored credentials have not changed, so
// the login last seen there is still the login: the account does not vanish.
func TestAnAccountStaysKnownWhileGwsCannotBeAsked(t *testing.T) {
	f := &fakeCLI{}
	f.on("gws", "", "auth status", reply{out: gwsStatus("priya@example.com", true)})
	h := hostWith(f, "/cfg/gws")
	h.Gog = ""
	h.Stamp = func(string) string { return "v1" }
	if d := h.Discover(context.Background()); len(d.Accounts) != 1 {
		t.Fatalf("first discovery: %v %v", d.Accounts, d.Errors)
	}

	f.rules = nil
	f.on("gws", "", "auth status", reply{errOut: "error: HTTP request failed: error sending request", code: 1})
	h.Forget()
	d := h.Discover(context.Background())
	if _, ok := d.Find("priya@example.com"); !ok {
		t.Fatalf("the account vanished when gws could not be asked: %v %v", d.Accounts, d.Errors)
	}

	// Changed credentials are a different login: the old answer is not reused.
	h.Stamp = func(string) string { return "v2" }
	h.Forget()
	if d := h.Discover(context.Background()); len(d.Accounts) != 0 || len(d.Errors) != 1 {
		t.Errorf("a login whose credentials changed was assumed: %v %v", d.Accounts, d.Errors)
	}
}

// With no login to fall back on, a discovery that could not look everywhere
// does not claim the account has no login, and the fix names a CLI the host
// has.
func TestAnAccountNotFoundIsUnknownOnlyWhenEveryLoginWasLookedAt(t *testing.T) {
	f := &fakeCLI{}
	f.on("gws", "", "auth status", reply{errOut: "error: HTTP request failed: error sending request", code: 1})
	h := hostWith(f, "/cfg/gws")
	h.Gog, h.DefaultGwsDir = "", "/cfg/gws"
	_, err := NewClient(h, time.UTC).Events(context.Background(), "priya@example.com", gcal.Query{})
	var ue *UnknownAccountError
	if err == nil || errors.As(err, &ue) {
		t.Errorf("gws could not be asked, yet the account was called unknown: %v", err)
	}

	f = &fakeCLI{}
	f.on("gws", "", "auth status", reply{out: gwsStatus("rahul@example.com", true)})
	h = hostWith(f, "/cfg/gws")
	h.Gog, h.DefaultGwsDir = "", "/cfg/gws"
	_, err = NewClient(h, time.UTC).Events(context.Background(), "priya@example.com", gcal.Query{})
	if !errors.As(err, &ue) {
		t.Fatalf("want an unknown account, got %v", err)
	}
	if strings.Contains(err.Error(), "gog") || !strings.Contains(err.Error(), "GOOGLE_WORKSPACE_CLI_CONFIG_DIR=/cfg/gws-priya gws auth login") {
		t.Errorf("the fix should use gws, the CLI this host has: %v", err)
	}
}
