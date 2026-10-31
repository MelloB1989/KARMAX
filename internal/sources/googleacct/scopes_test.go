package googleacct

import (
	"context"
	"errors"
	"strings"
	"testing"
)

const (
	scopeCal      = "https://www.googleapis.com/auth/calendar"
	scopeChat     = "https://www.googleapis.com/auth/chat.messages"
	scopeModify   = "https://www.googleapis.com/auth/gmail.modify"
	scopeReadonly = "https://www.googleapis.com/auth/gmail.readonly"
	scopeMeta     = "https://www.googleapis.com/auth/gmail.metadata"
	scopeSend     = "https://www.googleapis.com/auth/gmail.send"
)

func gwsStatusWith(user string, scopes ...string) string {
	q := make([]string, len(scopes))
	for i, s := range scopes {
		q[i] = `"` + s + `"`
	}
	return "Using keyring backend: file\n" + `{"has_refresh_token":true,"scopes":[` + strings.Join(q, ",") + `],"user":"` + user + `"}`
}

func gwsLoginWith(t *testing.T, scopes ...string) Login {
	t.Helper()
	f := &fakeCLI{}
	f.on("gog", "", "auth list", reply{out: `{"accounts":[]}`})
	f.on("gws", "", "auth status", reply{out: gwsStatusWith(acctAddr, scopes...)})
	h := hostWith(f, "/cfg/gws")
	h.DefaultGwsDir = "/cfg/gws"
	h.Env = []string{"GOOGLE_WORKSPACE_CLI_KEYRING_BACKEND=file"}
	d := h.Discover(context.Background())
	if len(d.Accounts) != 1 || len(d.Accounts[0].Logins) != 1 {
		t.Fatalf("discovery %+v", d)
	}
	return d.Accounts[0].Logins[0]
}

func cans(l Login) string {
	var out []string
	for _, s := range []string{GmailRead, GmailBody, GmailModify, GmailSend} {
		if l.Can(s) {
			out = append(out, s)
		}
	}
	return strings.Join(out, ",")
}

// What a login may do in Gmail is read from the scopes Google granted it:
// gmail.modify covers reading, labelling and sending; readonly only reading;
// metadata only headers; send only sending.
func TestAGwsLoginsGmailAccessFollowsItsScopes(t *testing.T) {
	for _, tc := range []struct {
		scopes []string
		want   string
	}{
		{[]string{scopeCal, scopeModify}, "gmail,gmail.body,gmail.modify,gmail.send"},
		{[]string{"https://mail.google.com/"}, "gmail,gmail.body,gmail.modify,gmail.send"},
		{[]string{scopeCal, scopeReadonly}, "gmail,gmail.body"},
		{[]string{scopeMeta}, "gmail"},
		{[]string{scopeMeta, scopeSend}, "gmail,gmail.send"},
		{[]string{"https://www.googleapis.com/auth/gmail.compose"}, "gmail.send"},
		{[]string{scopeCal, scopeChat}, ""},
	} {
		if got := cans(gwsLoginWith(t, tc.scopes...)); got != tc.want {
			t.Errorf("scopes %v: can %q, want %q", tc.scopes, got, tc.want)
		}
	}
}

// A gws login missing a Gmail scope is fixed by signing in again with every
// scope it already holds plus gmail.modify — exactly, so the fix does not
// cost the calendar or Chat access it has.
func TestTheFixForAMissingGmailScopeKeepsEveryOtherScope(t *testing.T) {
	l := gwsLoginWith(t, "openid", "email", scopeCal, scopeChat, scopeReadonly)
	want := "GOOGLE_WORKSPACE_CLI_KEYRING_BACKEND=file gws auth login --scopes " +
		scopeCal + "," + scopeChat + "," + scopeModify
	if got := l.FixFor(GmailSend); got != want {
		t.Errorf("fix for send:\n got %q\nwant %q", got, want)
	}
	if got := l.FixFor("calendar"); got != "GOOGLE_WORKSPACE_CLI_KEYRING_BACKEND=file gws auth login" {
		t.Errorf("a login that holds calendar is fixed by signing in again: %q", got)
	}
	if l.Fix() != l.FixFor("") {
		t.Error("Fix is the fix for no particular service")
	}
}

// gog names its grants by service. A login that has Gmail can do all of it
// (gog asks for gmail.modify), and a fix keeps the services it already has.
func TestAGogLoginsGmailAccessAndFix(t *testing.T) {
	f := &fakeCLI{}
	f.on("gog", "", "auth list", reply{out: `{"accounts":[
	 {"email":"priya@example.com","services":["calendar","chat"],"auth":"oauth"},
	 {"email":"dev@example.net","services":["gmail","calendar"],"auth":"oauth"},
	 {"email":"ro@example.net","services":["gmail"],"scopes":["` + scopeReadonly + `"],"auth":"oauth"}]}`})
	h := hostWith(f)
	h.Gws = ""
	d := h.Discover(context.Background())
	priya, _ := d.Find("priya@example.com")
	dev, _ := d.Find("dev@example.net")
	ro, _ := d.Find("ro@example.net")
	if got := cans(priya.Logins[0]); got != "" {
		t.Errorf("a gog login without gmail can %q", got)
	}
	if got := priya.Logins[0].FixFor(GmailRead); got != "gog auth add priya@example.com --services calendar,chat,gmail --force-consent" {
		t.Errorf("gog fix: %q", got)
	}
	if got := cans(dev.Logins[0]); got != "gmail,gmail.body,gmail.modify,gmail.send" {
		t.Errorf("gog with gmail can %q", got)
	}
	if got := cans(ro.Logins[0]); got != "gmail,gmail.body" {
		t.Errorf("gog read-only gmail can %q", got)
	}
	if got := ro.Logins[0].FixFor(GmailSend); got != "gog auth add ro@example.net --services gmail --gmail-scope full --force-consent" {
		t.Errorf("gog read-only fix: %q", got)
	}
}

// An account lacks a Gmail power when none of its working logins has it, and
// its fix is the best login's.
func TestAnAccountSaysWhatItLacksAndHowToFixIt(t *testing.T) {
	l := gwsLoginWith(t, scopeCal, scopeReadonly)
	a := Account{Email: acctAddr, Logins: []Login{l}}
	if a.Lacks(GmailRead) || a.Lacks(GmailBody) || !a.Lacks(GmailSend) || !a.Lacks(GmailModify) {
		t.Fatalf("lacks: read %v body %v send %v modify %v", a.Lacks(GmailRead), a.Lacks(GmailBody), a.Lacks(GmailSend), a.Lacks(GmailModify))
	}
	if fix := a.FixFor(GmailSend); !strings.Contains(fix, scopeModify) || !strings.Contains(fix, scopeCal) {
		t.Errorf("account fix %q", fix)
	}
	broken := Account{Email: acctAddr, Logins: []Login{{Email: acctAddr, Path: Gws, Problem: "signed out", Services: []string{GmailRead, GmailSend}}}}
	if !broken.Lacks(GmailSend) {
		t.Error("a signed-out login grants nothing")
	}
}

// A call that needs a power no login holds is refused with the command that
// grants it.
func TestACallNeedingAMissingScopeSaysWhatToRun(t *testing.T) {
	f := &fakeCLI{}
	f.on("gog", "", "auth list", reply{out: `{"accounts":[]}`})
	f.on("gws", "", "auth status", reply{out: gwsStatusWith(acctAddr, scopeCal, scopeReadonly)})
	c := NewClient(hostWith(f, "/cfg/gws"), nil)
	err := c.with(context.Background(), acctAddr, GmailSend, func(Login) error {
		t.Fatal("ran through a login without the scope")
		return nil
	})
	var ae *AuthError
	if !errors.As(err, &ae) || !strings.Contains(err.Error(), "--scopes") || !strings.Contains(err.Error(), scopeModify) {
		t.Fatalf("err %v", err)
	}
}
