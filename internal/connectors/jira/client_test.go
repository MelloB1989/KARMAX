package jira

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/MelloB1989/karmax/pkg/connectorkit"
)

func creds(cfg map[string]string) connectorkit.Credentials {
	return connectorkit.Credentials{Config: cfg}
}

// setAtlassianAPI points the OAuth-side host at a stub and returns the
// previous value, so a test can restore it in one deferred line.
func setAtlassianAPI(u string) string {
	old := atlassianAPI
	atlassianAPI = u
	return old
}

func contains(h, n string) bool { return strings.Contains(h, n) }

func TestBaseAndAuthPrefersAPIToken(t *testing.T) {
	c := creds(map[string]string{"email": "a@b.com", "api_token": "tok", "site_url": "acme.atlassian.net/"})
	base, auth, err := baseAndAuth(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	if base != "https://acme.atlassian.net" {
		t.Errorf("base = %q, want a normalised https origin with no trailing slash", base)
	}
	if !strings.HasPrefix(auth, "Basic ") {
		t.Errorf("auth = %q, want Basic", auth)
	}
}

func TestBaseAndAuthWithNeitherConfiguredFailsBeforeAnyRequest(t *testing.T) {
	if _, _, err := baseAndAuth(context.Background(), creds(nil)); err == nil {
		t.Error("a call with no credentials at all was accepted")
	}
}

func TestBaseAndAuthUsesOAuthWhenAnAccessTokenIsPresent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]accessibleResource{{ID: "cloud-1", URL: "https://acme.atlassian.net"}})
	}))
	defer srv.Close()
	defer setAtlassianAPI(setAtlassianAPI(srv.URL))

	c := connectorkit.Credentials{AccessToken: "tok"}
	base, auth, err := baseAndAuth(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	if base != srv.URL+"/ex/jira/cloud-1" {
		t.Errorf("base = %q", base)
	}
	if auth != "Bearer tok" {
		t.Errorf("auth = %q", auth)
	}
}

func TestResolveCloudDisambiguatesWithSiteHint(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]accessibleResource{
			{ID: "1", URL: "https://one.atlassian.net"},
			{ID: "2", URL: "https://two.atlassian.net"},
		})
	}))
	defer srv.Close()
	defer setAtlassianAPI(setAtlassianAPI(srv.URL))

	if _, err := resolveCloud(context.Background(), "tok", ""); err == nil {
		t.Error("several accessible sites with no hint was accepted")
	}
	id, err := resolveCloud(context.Background(), "tok", "https://two.atlassian.net/")
	if err != nil || id != "2" {
		t.Errorf("id=%q err=%v", id, err)
	}
	if _, err := resolveCloud(context.Background(), "tok", "https://nope.atlassian.net"); err == nil {
		t.Error("a hint matching nothing was accepted")
	}
}

func TestResolveCloudRejectsAnUnauthorisedToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	defer setAtlassianAPI(setAtlassianAPI(srv.URL))

	if _, err := resolveCloud(context.Background(), "bad", ""); err == nil || !contains(err.Error(), "rejected") {
		t.Errorf("err = %v", err)
	}
}

func TestCallMapsStatusCodesToReadableCauses(t *testing.T) {
	cases := []struct {
		status int
		body   string
		want   string
	}{
		{http.StatusUnauthorized, "", "credentials were rejected"},
		{http.StatusForbidden, "", "not permitted"},
		{http.StatusNotFound, "", "not found"},
		{http.StatusTooManyRequests, "", "rate limited"},
	}
	for _, tc := range cases {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if tc.status == http.StatusTooManyRequests {
				w.Header().Set("Retry-After", "30")
			}
			w.WriteHeader(tc.status)
			_, _ = w.Write([]byte(tc.body))
		}))
		c := creds(map[string]string{"email": "a@b.com", "api_token": "t", "site_url": srv.URL})
		_, err := call(context.Background(), c, http.MethodGet, "/rest/api/3/myself", nil, nil)
		srv.Close()
		if err == nil || !contains(err.Error(), tc.want) {
			t.Errorf("status %d: err = %v, want it to name %q", tc.status, err, tc.want)
		}
	}
}

func TestCallReportsAnUnreachableSiteDistinctlyFromBadCredentials(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	srv.Close() // closed: nothing is listening, so the request fails to connect at all

	c := creds(map[string]string{"email": "a@b.com", "api_token": "t", "site_url": srv.URL})
	_, err := call(context.Background(), c, http.MethodGet, "/rest/api/3/myself", nil, nil)
	if err == nil || !contains(err.Error(), "could not reach") {
		t.Errorf("err = %v, want an unreachable-site message", err)
	}
}

func TestCallDecodesASuccessfulResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); !strings.HasPrefix(got, "Basic ") {
			t.Errorf("auth header = %q", got)
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"accountId": "abc", "displayName": "Mia"})
	}))
	defer srv.Close()

	c := creds(map[string]string{"email": "a@b.com", "api_token": "t", "site_url": srv.URL})
	var out struct {
		AccountID   string `json:"accountId"`
		DisplayName string `json:"displayName"`
	}
	if _, err := call(context.Background(), c, http.MethodGet, "/rest/api/3/myself", nil, &out); err != nil {
		t.Fatal(err)
	}
	if out.AccountID != "abc" || out.DisplayName != "Mia" {
		t.Errorf("decoded %+v", out)
	}
}

func TestAdfTextHandlesBothShapesJiraSends(t *testing.T) {
	if got := adfText(json.RawMessage(`"plain text"`)); got != "plain text" {
		t.Errorf("plain string: got %q", got)
	}
	adf := `{"type":"doc","version":1,"content":[
		{"type":"paragraph","content":[{"type":"text","text":"line one"}]},
		{"type":"paragraph","content":[{"type":"text","text":"line two"}]}
	]}`
	got := adfText(json.RawMessage(adf))
	if !contains(got, "line one") || !contains(got, "line two") {
		t.Errorf("ADF flatten: got %q", got)
	}
	if got := adfText(nil); got != "" {
		t.Errorf("nil input: got %q", got)
	}
	if got := adfText(json.RawMessage(`{not json`)); got != "" {
		t.Errorf("malformed input should not error the caller: got %q", got)
	}
}

func TestPlainADFWrapsTextForTheV3API(t *testing.T) {
	doc := plainADF("hello")
	b, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	var back struct {
		Type    string `json:"type"`
		Content []struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"content"`
	}
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if back.Type != "doc" || back.Content[0].Content[0].Text != "hello" {
		t.Errorf("round trip = %+v", back)
	}
}

func TestParseJiraTime(t *testing.T) {
	got, err := parseJiraTime("2024-01-02T15:04:05.123+0000")
	if err != nil {
		t.Fatal(err)
	}
	if got.Year() != 2024 || got.Month() != 1 || got.Day() != 2 {
		t.Errorf("parsed %v", got)
	}
	if _, err := parseJiraTime(""); err == nil {
		t.Error("an empty timestamp was accepted")
	}
}
