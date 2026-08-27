package jira

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/MelloB1989/karmax/pkg/connectorkit"
)

func TestSourcesDeclareTheThreeRequiredEventKindsOnDistinctPaths(t *testing.T) {
	srcs := New("").Sources()
	kinds := map[string]bool{}
	paths := map[string]bool{}
	for _, s := range srcs {
		kinds[s.EventKind] = true
		if s.Kind == connectorkit.SourceWebhook {
			if paths[s.Path] {
				t.Errorf("path %q registered twice — the webhook server cannot route it", s.Path)
			}
			paths[s.Path] = true
		}
	}
	for _, want := range []string{EventIssueCreated, EventIssueUpdated, EventCommentCreated} {
		if !kinds[want] {
			t.Errorf("no source publishes %q", want)
		}
	}
}

func TestSourcesQualifyPathsByAccountSoTwoSitesDoNotCollide(t *testing.T) {
	a, b := New("prod").Sources(), New("staging").Sources()
	for i := range a {
		if a[i].Kind != connectorkit.SourceWebhook {
			continue
		}
		if a[i].Path == b[i].Path {
			t.Errorf("two accounts share path %q", a[i].Path)
		}
	}
}

func TestReconcileIsAPollNotAWebhook(t *testing.T) {
	for _, s := range New("").Sources() {
		if s.ID == reconcileID && s.Kind != connectorkit.SourcePoll {
			t.Errorf("reconcile source has kind %v, want SourcePoll", s.Kind)
		}
	}
}

func TestHealthDistinguishesBadCredentialsFromAnUnreachableSite(t *testing.T) {
	unauthorized := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer unauthorized.Close()

	conn := New("")
	err := conn.Health(context.Background(), creds(map[string]string{
		"email": "a@b.com", "api_token": "bad", "site_url": unauthorized.URL,
	}))
	if err == nil || !contains(err.Error(), "rejected") {
		t.Errorf("bad credentials: err = %v", err)
	}

	unreachable := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	unreachable.Close()
	err = conn.Health(context.Background(), creds(map[string]string{
		"email": "a@b.com", "api_token": "t", "site_url": unreachable.URL,
	}))
	if err == nil || !contains(err.Error(), "could not reach") {
		t.Errorf("unreachable site: err = %v", err)
	}
}

func TestHealthRejectsAResponseWithNoAccountID(t *testing.T) {
	// A site that answers 200 with something that isn't really a Jira "myself"
	// response — the wrong-site case, where the URL resolves but is not this
	// account's Jira.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	err := New("").Health(context.Background(), creds(map[string]string{
		"email": "a@b.com", "api_token": "t", "site_url": srv.URL,
	}))
	if err == nil || !contains(err.Error(), "did not identify") {
		t.Errorf("err = %v", err)
	}
}

func TestNameQualifiesToolsByAccount(t *testing.T) {
	primary, secondary := New(""), New("staging")
	if primary.name("jira.search") != "jira.search" {
		t.Errorf("primary name = %q", primary.name("jira.search"))
	}
	if secondary.name("jira.search") != "jira.search@staging" {
		t.Errorf("secondary name = %q", secondary.name("jira.search"))
	}
}
