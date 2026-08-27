package google

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/MelloB1989/karmax/pkg/connectorkit"
)

func creds(kv map[string]string) connectorkit.Credentials {
	return connectorkit.Credentials{Config: kv}
}

// The whole design: without PerUser one person's token would serve everybody.
func TestTheConnectorDeclaresItselfPerUser(t *testing.T) {
	m := New().Manifest()
	if !m.PerUser {
		t.Fatal("google is not marked PerUser — the whole company would read one inbox")
	}
	for _, f := range m.Config {
		if f.Key == "client_secret" && !f.Secret {
			t.Error("client_secret is not marked secret")
		}
	}
}

// access_type=offline and prompt=consent together are what produce a refresh
// token. Without them a returning user gets an hour of access and no way back,
// which presents later as "it worked yesterday".
func TestConsentURLAsksForOfflineAccess(t *testing.T) {
	raw, err := AuthCodeURL(creds(map[string]string{"client_id": "cid"}), "https://x/cb", "st")
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()

	if q.Get("access_type") != "offline" {
		t.Error("access_type is not offline — no refresh token would be issued")
	}
	if q.Get("prompt") != "consent" {
		t.Error("prompt is not consent — a returning user gets no refresh token")
	}
	if q.Get("state") != "st" || q.Get("redirect_uri") != "https://x/cb" {
		t.Errorf("state or redirect missing: %v", q)
	}
	for _, want := range []string{"gmail.modify", "calendar.events", "drive.readonly", "chat.messages"} {
		if !strings.Contains(q.Get("scope"), want) {
			t.Errorf("scope %q not requested", want)
		}
	}
}

func TestConsentURLRefusesWithoutSetup(t *testing.T) {
	if _, err := AuthCodeURL(creds(map[string]string{}), "https://x/cb", "s"); err == nil {
		t.Error("a consent URL was built with no client_id")
	}
	if _, err := AuthCodeURL(creds(map[string]string{"client_id": "c"}), "", "s"); err == nil {
		t.Error("a consent URL was built with no redirect URI")
	}
}

// Restricting to a Workspace domain is the difference between "our staff" and
// "anyone with a Google account".
func TestHostedDomainIsPassedThroughWhenSet(t *testing.T) {
	raw, _ := AuthCodeURL(creds(map[string]string{"client_id": "c", "hosted_domain": "acme.com"}), "https://x/cb", "s")
	if !strings.Contains(raw, "hd=acme.com") {
		t.Error("hosted_domain was not applied")
	}
	raw, _ = AuthCodeURL(creds(map[string]string{"client_id": "c"}), "https://x/cb", "s")
	if strings.Contains(raw, "hd=") {
		t.Error("an empty hosted_domain was sent, which would restrict sign-in to nothing")
	}
}

// A grant that has gone stale must say what to do, not just fail.
func TestInvalidGrantIsExplained(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":"invalid_grant","error_description":"Token has been expired or revoked."}`))
	}))
	defer srv.Close()
	old := tokenEndpoint
	tokenEndpoint = srv.URL
	defer func() { tokenEndpoint = old }()

	_, err := Refresh(context.Background(), creds(map[string]string{"client_id": "c", "client_secret": "s"}), "rt")
	if err == nil || !strings.Contains(err.Error(), "connect again") {
		t.Errorf("unhelpful message for a dead grant: %v", err)
	}
}

// Storing a connection with no refresh token means one that dies in an hour and
// looks fine until it does.
func TestExchangeComplainsWhenNoRefreshTokenComesBack(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"access_token":"at","expires_in":3600,"scope":"a b"}`))
	}))
	defer srv.Close()
	old := tokenEndpoint
	tokenEndpoint = srv.URL
	defer func() { tokenEndpoint = old }()

	_, err := Exchange(context.Background(), creds(map[string]string{"client_id": "c", "client_secret": "s"}), "code", "https://x/cb")
	if err == nil || !strings.Contains(err.Error(), "revoke it") {
		t.Errorf("a missing refresh token was accepted silently: %v", err)
	}
}

// A Gmail message is a tree, not a string. Reading only the top level returns
// "" for most real mail.
func TestMailBodyIsFoundInNestedParts(t *testing.T) {
	enc := func(s string) string {
		return base64.URLEncoding.WithPadding(base64.NoPadding).EncodeToString([]byte(s))
	}
	raw := `{"payload":{"mimeType":"multipart/mixed","parts":[
		{"mimeType":"multipart/alternative","parts":[
			{"mimeType":"text/plain","body":{"data":"` + enc("the actual text") + `"}},
			{"mimeType":"text/html","body":{"data":"` + enc("<p>ignored</p>") + `"}}
		]},
		{"mimeType":"application/pdf","body":{"data":""}}
	]}}`
	var m gmailMessage
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		t.Fatal(err)
	}
	if got := m.body(); got != "the actual text" {
		t.Errorf("nested body not found, got %q", got)
	}
}

// When there is no text/plain at all, HTML is rendered rather than handed over
// as markup that eats the context window.
func TestHTMLOnlyMailIsRenderedAsText(t *testing.T) {
	enc := base64.URLEncoding.WithPadding(base64.NoPadding).EncodeToString(
		[]byte(`<html><style>p{color:red}</style><body><p>Hello&nbsp;there</p></body></html>`))
	var m gmailMessage
	if err := json.Unmarshal([]byte(`{"payload":{"mimeType":"text/html","body":{"data":"`+enc+`"}}}`), &m); err != nil {
		t.Fatal(err)
	}
	got := m.body()
	if strings.Contains(got, "<") || strings.Contains(got, "color:red") {
		t.Errorf("markup survived: %q", got)
	}
	if !strings.Contains(got, "Hello there") {
		t.Errorf("text was lost: %q", got)
	}
}

// An apostrophe in a filename otherwise terminates the Drive query string and
// produces a syntax error rather than a search.
func TestDriveQueryEscapesApostrophes(t *testing.T) {
	var seen string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.URL.Query().Get("q")
		w.Write([]byte(`{"files":[]}`))
	}))
	defer srv.Close()
	old := driveAPI
	driveAPI = srv.URL
	defer func() { driveAPI = old }()

	cr := connectorkit.Credentials{AccessToken: "at", Config: map[string]string{}}
	if _, err := driveSearch(context.Background(), cr, map[string]any{"query": "Priya's plan"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(seen, `Priya\'s plan`) {
		t.Errorf("apostrophe not escaped: %q", seen)
	}
}

// A Google-native file has no bytes to download; it must be EXPORTED. Getting
// this backwards returns "Only files with binary content can be downloaded".
func TestGoogleNativeFilesAreExportedNotDownloaded(t *testing.T) {
	var path, query string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/export") || r.URL.Query().Get("alt") == "media" {
			path, query = r.URL.Path, r.URL.RawQuery
			w.Write([]byte("contents"))
			return
		}
		w.Write([]byte(`{"name":"Plan","mimeType":"application/vnd.google-apps.document"}`))
	}))
	defer srv.Close()
	old := driveAPI
	driveAPI = srv.URL
	defer func() { driveAPI = old }()

	cr := connectorkit.Credentials{AccessToken: "at", Config: map[string]string{}}
	if _, err := driveRead(context.Background(), cr, map[string]any{"id": "f1"}); err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(path, "/export") {
		t.Errorf("a Doc was downloaded rather than exported: %s", path)
	}
	if !strings.Contains(query, "mimeType=text%2Fplain") {
		t.Errorf("no export format requested: %s", query)
	}
}

// An all-day event has `date` and no `dateTime`; reporting "" would make a real
// event look malformed.
func TestAllDayEventsKeepTheirDate(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"items":[{"summary":"Offsite","start":{"date":"2026-09-01"},"end":{"date":"2026-09-02"}}]}`))
	}))
	defer srv.Close()
	old := calendarAPI
	calendarAPI = srv.URL
	defer func() { calendarAPI = old }()

	cr := connectorkit.Credentials{AccessToken: "at", Config: map[string]string{}}
	res, err := calendarList(context.Background(), cr, map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	ev := res.(map[string]any)["events"].([]map[string]any)[0]
	if ev["start"] != "2026-09-01" || ev["all_day"] != true {
		t.Errorf("all-day event mangled: %#v", ev)
	}
}

// "403 forbidden" on a per-user connector is ambiguous in the one way that
// matters: whose access failed.
func TestErrorsNameThePerson(t *testing.T) {
	cr := connectorkit.Credentials{Member: "kartik", Account: "k@acme.com"}
	err := apiError(http.StatusForbidden, []byte(`{"error":{"message":"insufficient scope"}}`), cr)
	if !strings.Contains(err.Error(), "k@acme.com") {
		t.Errorf("the error does not say whose access failed: %v", err)
	}
	err = apiError(http.StatusUnauthorized, []byte(`{}`), cr)
	if !strings.Contains(err.Error(), "reconnect") {
		t.Errorf("a dead sign-in should say to reconnect: %v", err)
	}
}

// A model asked for "tomorrow" sends a bare date as often as a full timestamp.
func TestDatesAreAcceptedInBothShapes(t *testing.T) {
	for _, given := range []string{"2026-09-01", "2026-09-01T10:00:00Z"} {
		if _, err := timeArg(map[string]any{"from": given}, "from", time.Time{}); err != nil {
			t.Errorf("%q rejected: %v", given, err)
		}
	}
	if _, err := timeArg(map[string]any{"from": "next tuesday"}, "from", time.Time{}); err == nil {
		t.Error("an unparseable date was accepted")
	}
}
