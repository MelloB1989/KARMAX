package github

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/MelloB1989/karmax/pkg/connectorkit"
)

func creds(cfg map[string]string) connectorkit.Credentials {
	return connectorkit.Credentials{Config: cfg}
}

func sign(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func TestAnUnsignedDeliveryIsRefused(t *testing.T) {
	body := []byte(`{"action":"opened"}`)
	c := creds(map[string]string{"webhook_secret": "s3cret"})

	if verifyDelivery(c, map[string]string{}, body) {
		t.Error("a delivery with no signature was accepted")
	}
	if verifyDelivery(c, map[string]string{"X-Hub-Signature-256": "sha256=deadbeef"}, body) {
		t.Error("a delivery with a wrong signature was accepted")
	}
	if !verifyDelivery(c, map[string]string{"X-Hub-Signature-256": sign("s3cret", body)}, body) {
		t.Error("a correctly signed delivery was refused")
	}
	// Tampering after signing must not verify.
	if verifyDelivery(c, map[string]string{"X-Hub-Signature-256": sign("s3cret", body)}, []byte(`{"action":"closed"}`)) {
		t.Error("a tampered body verified")
	}
}

// With no secret the endpoint would accept anything, and those events trigger
// loops — so it must refuse rather than default to open.
func TestNoWebhookSecretMeansNoDeliveries(t *testing.T) {
	body := []byte(`{}`)
	if verifyDelivery(creds(map[string]string{}), map[string]string{
		"X-Hub-Signature-256": sign("", body)}, body) {
		t.Error("deliveries were accepted with no secret configured")
	}
}

func TestOnlyInterestingDeliveriesBecomeEvents(t *testing.T) {
	body := []byte(`{"action":"opened","repository":{"full_name":"o/r"},"sender":{"login":"someone"},
	                 "issue":{"number":7,"title":"it breaks","body":"steps to reproduce","html_url":"https://x/7"}}`)

	events, err := decodeDelivery(map[string]string{"X-GitHub-Event": "issues"}, body)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 {
		t.Fatalf("got %d events", len(events))
	}
	e := events[0]
	if e["number"] != 7 || e["repo"] != "o/r" || e["actor"] != "someone" {
		t.Errorf("decoded %+v", e)
	}

	// A push or a star is noise; publishing it would make the log useless for
	// the things that matter.
	for _, kind := range []string{"push", "star", "watch", "ping"} {
		got, err := decodeDelivery(map[string]string{"X-GitHub-Event": kind}, body)
		if err != nil || len(got) != 0 {
			t.Errorf("%s produced %d events (err %v)", kind, len(got), err)
		}
	}
}

func TestAPullRequestDeliveryPrefersThePullRequest(t *testing.T) {
	body := []byte(`{"action":"review_requested","repository":{"full_name":"o/r"},"sender":{"login":"x"},
	                 "pull_request":{"number":42,"title":"add the thing","body":"why","html_url":"https://x/42","draft":true}}`)
	events, err := decodeDelivery(map[string]string{"X-GitHub-Event": "pull_request"}, body)
	if err != nil || len(events) != 1 {
		t.Fatalf("events=%v err=%v", events, err)
	}
	if events[0]["number"] != 42 || events[0]["draft"] != true {
		t.Errorf("decoded %+v", events[0])
	}
}

func TestARepositoryIsRequiredAndNormalised(t *testing.T) {
	c := creds(map[string]string{"default_repo": "MelloB1989/karmax"})
	if _, err := repoOf(creds(map[string]string{}), map[string]any{}); err == nil {
		t.Error("a call with no repo anywhere was accepted")
	}
	got, err := repoOf(c, map[string]any{})
	if err != nil || got != "MelloB1989/karmax" {
		t.Errorf("default repo = %q, err %v", got, err)
	}
	got, err = repoOf(c, map[string]any{"repo": "https://github.com/other/repo/"})
	if err != nil || got != "other/repo" {
		t.Errorf("a pasted URL normalised to %q, err %v", got, err)
	}
}

// GitHub answers an exhausted quota with 403 and a reset header, not 429. A
// client that reads that as "forbidden" gives up on a credential that is fine.
func TestAnExhaustedRateLimitIsReportedAsSuchNotAsForbidden(t *testing.T) {
	reset := time.Now().Add(20 * time.Minute)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(reset.Unix(), 10))
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"message":"API rate limit exceeded"}`))
	}))
	defer srv.Close()

	defer setAPI(setAPI(srv.URL))
	rl, err := call(context.Background(), creds(map[string]string{"token": "t"}), http.MethodGet, "/user", nil, nil)
	if err == nil {
		t.Fatal("an exhausted rate limit was not reported as an error")
	}
	if !contains(err.Error(), "rate limit") {
		t.Errorf("error does not name the cause: %v", err)
	}
	if rl.Remaining != 0 || rl.ResetAt.Unix() != reset.Unix() {
		t.Errorf("rate limit not parsed: %+v", rl)
	}
}

func TestARejectedTokenSaysSo(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	defer setAPI(setAPI(srv.URL))
	_, err := call(context.Background(), creds(map[string]string{"token": "bad"}), http.MethodGet, "/user", nil, nil)
	if err == nil || !contains(err.Error(), "token was rejected") {
		t.Errorf("an expired token produced: %v", err)
	}
}

func TestAMissingTokenFailsBeforeAnyRequest(t *testing.T) {
	if _, err := call(context.Background(), creds(map[string]string{}), http.MethodGet, "/user", nil, nil); err == nil {
		t.Error("a call with no token was attempted")
	}
}

func TestListIssuesShapesTheResult(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer t" {
			t.Errorf("auth header = %q", got)
		}
		_ = json.NewEncoder(w).Encode([]map[string]any{
			{"number": 1, "title": "a bug", "html_url": "https://x/1", "state": "open",
				"comments": 2, "user": map[string]any{"login": "someone"}},
			{"number": 2, "title": "a pr", "html_url": "https://x/2", "state": "open",
				"pull_request": map[string]any{}, "user": map[string]any{"login": "other"}},
		})
	}))
	defer srv.Close()

	defer setAPI(setAPI(srv.URL))

	out, err := listIssues(context.Background(),
		creds(map[string]string{"token": "t", "default_repo": "o/r"}), map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	m := out.(map[string]any)
	if m["count"] != 2 {
		t.Errorf("count = %v", m["count"])
	}
	items := m["items"].([]map[string]any)
	if items[0]["is_pull_request"] != false || items[1]["is_pull_request"] != true {
		t.Errorf("issues and PRs not distinguished: %+v", items)
	}
}

// setAPI points the connector at a stub and returns the previous value, so a
// test can restore it in one deferred line.
func setAPI(u string) string {
	old := api
	api = u
	return old
}

func contains(h, n string) bool {
	for i := 0; i+len(n) <= len(h); i++ {
		if h[i:i+len(n)] == n {
			return true
		}
	}
	return false
}
