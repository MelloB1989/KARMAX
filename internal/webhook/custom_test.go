package webhook

import (
	"crypto/hmac"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MelloB1989/karmax/internal/bus"
	"github.com/MelloB1989/karmax/internal/store"
	"go.uber.org/zap"
)

func dispatcher(t *testing.T) (*CustomDispatcher, *store.Store) {
	t.Helper()
	db, err := store.New(filepath.Join(t.TempDir(), "w.db"), zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return NewCustomDispatcher(db, bus.NewLog(db, store.DefaultWorkspace, zap.NewNop()), zap.NewNop()), db
}

func post(d *CustomDispatcher, slug, body string, headers map[string]string) *httptest.ResponseRecorder {
	mux := http.NewServeMux()
	d.Mount(func(p string, h http.HandlerFunc) { mux.HandleFunc(p, h) })
	r := httptest.NewRequest("POST", CustomPrefix+slug, strings.NewReader(body))
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	return w
}

func endpoint(t *testing.T, db *store.Store, e store.WebhookEndpoint) store.WebhookEndpoint {
	t.Helper()
	if e.EventKind == "" {
		e.EventKind = "custom.test"
	}
	saved, err := db.SaveWebhookEndpoint(e)
	if err != nil {
		t.Fatal(err)
	}
	return saved
}

// The point of the whole design: an endpoint edited while running takes effect
// on the next delivery, with no restart. AddRoute cannot do this — the mux
// panics on a repeated pattern.
func TestAnEndpointCanBeChangedWhileRunning(t *testing.T) {
	d, db := dispatcher(t)
	e := endpoint(t, db, store.WebhookEndpoint{Slug: "stripe", Enabled: true})

	if w := post(d, "stripe", `{"a":1}`, nil); w.Code != http.StatusOK {
		t.Fatalf("first delivery: %d %s", w.Code, w.Body.String())
	}

	// Turn it off — no restart, no re-mount.
	e.Enabled = false
	if _, err := db.SaveWebhookEndpoint(e); err != nil {
		t.Fatal(err)
	}
	if w := post(d, "stripe", `{"a":1}`, nil); w.Code != http.StatusServiceUnavailable {
		t.Errorf("a disabled endpoint still accepted a delivery: %d", w.Code)
	}

	// And a secret added afterwards is enforced immediately.
	e.Enabled, e.Secret = true, "s3cret"
	if _, err := db.SaveWebhookEndpoint(e); err != nil {
		t.Fatal(err)
	}
	if w := post(d, "stripe", `{"a":1}`, nil); w.Code != http.StatusUnauthorized {
		t.Errorf("a newly added secret was not enforced: %d", w.Code)
	}
}

// GitHub sends sha256=, older integrations sha1=, smaller services bare hex.
func TestSignaturesAreAcceptedInEveryShapeSendersUse(t *testing.T) {
	d, db := dispatcher(t)
	endpoint(t, db, store.WebhookEndpoint{
		Slug: "signed", Enabled: true, Secret: "s3cret", SignatureHeader: "X-Hub-Signature-256",
	})
	body := `{"hello":"world"}`

	sha256sum := func() string {
		m := hmac.New(sha256.New, []byte("s3cret"))
		m.Write([]byte(body))
		return hex.EncodeToString(m.Sum(nil))
	}()
	sha1sum := func() string {
		m := hmac.New(sha1.New, []byte("s3cret"))
		m.Write([]byte(body))
		return hex.EncodeToString(m.Sum(nil))
	}()

	for name, sig := range map[string]string{
		"github style": "sha256=" + sha256sum,
		"bare hex":     sha256sum,
		"sha1 style":   "sha1=" + sha1sum,
		"upper case":   "sha256=" + strings.ToUpper(sha256sum),
	} {
		w := post(d, "signed", body, map[string]string{"X-Hub-Signature-256": sig})
		if w.Code != http.StatusOK {
			t.Errorf("%s was rejected: %d %s", name, w.Code, w.Body.String())
		}
	}

	// And a wrong one is refused.
	if w := post(d, "signed", body, map[string]string{"X-Hub-Signature-256": "sha256=" + strings.Repeat("0", 64)}); w.Code != http.StatusUnauthorized {
		t.Errorf("a bad signature was accepted: %d", w.Code)
	}
	// A signature over a DIFFERENT body must not verify.
	if w := post(d, "signed", `{"hello":"tampered"}`, map[string]string{"X-Hub-Signature-256": "sha256=" + sha256sum}); w.Code != http.StatusUnauthorized {
		t.Errorf("a signature from another body was accepted: %d", w.Code)
	}
	// A missing header is refused rather than treated as unsigned.
	if w := post(d, "signed", body, nil); w.Code != http.StatusUnauthorized {
		t.Errorf("a missing signature was accepted: %d", w.Code)
	}
}

func TestSharedTokenModeAcceptsTheUsualPlaces(t *testing.T) {
	d, db := dispatcher(t)
	endpoint(t, db, store.WebhookEndpoint{Slug: "tok", Enabled: true, Secret: "s3cret"})

	for name, h := range map[string]map[string]string{
		"dedicated header": {"X-Webhook-Token": "s3cret"},
		"bearer":           {"Authorization": "Bearer s3cret"},
	} {
		if w := post(d, "tok", `{}`, h); w.Code != http.StatusOK {
			t.Errorf("%s was rejected: %d", name, w.Code)
		}
	}
	if w := post(d, "tok", `{}`, map[string]string{"X-Webhook-Token": "wrong"}); w.Code != http.StatusUnauthorized {
		t.Errorf("a wrong token was accepted: %d", w.Code)
	}
}

// "Nothing is happening" and "everything is arriving with a bad signature" look
// identical from the operator's side without a record.
func TestRejectionsAreRecordedToo(t *testing.T) {
	d, db := dispatcher(t)
	endpoint(t, db, store.WebhookEndpoint{Slug: "audited", Enabled: true, Secret: "s3cret"})

	post(d, "audited", `{}`, map[string]string{"X-Webhook-Token": "s3cret"})
	post(d, "audited", `{}`, map[string]string{"X-Webhook-Token": "wrong"})

	got, err := db.WebhookDeliveries("audited", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 deliveries, got %d", len(got))
	}
	var statuses []string
	for _, g := range got {
		statuses = append(statuses, g.Status)
	}
	joined := strings.Join(statuses, ",")
	if !strings.Contains(joined, "accepted") || !strings.Contains(joined, "rejected") {
		t.Errorf("both outcomes should be recorded, got %s", joined)
	}
}

// A webhook that only accepts JSON rejects half of what real services send.
func TestNonJSONBodiesAreCarriedRatherThanRefused(t *testing.T) {
	d, db := dispatcher(t)
	endpoint(t, db, store.WebhookEndpoint{Slug: "anything", Enabled: true})

	for name, body := range map[string]string{
		"form encoded": "a=1&b=2",
		"plain text":   "just some text",
		"json array":   `[{"a":1}]`,
	} {
		if w := post(d, "anything", body, nil); w.Code != http.StatusOK {
			t.Errorf("%s was refused: %d", name, w.Code)
		}
	}
}

// An unknown slug must not create a row, or anyone could fill the table by
// guessing paths.
func TestAnUnknownSlugIsNotRecorded(t *testing.T) {
	d, db := dispatcher(t)
	if w := post(d, "never-created", `{}`, nil); w.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", w.Code)
	}
	got, _ := db.WebhookDeliveries("", 10)
	if len(got) != 0 {
		t.Errorf("a probe at an unknown path wrote %d rows", len(got))
	}
}

func TestSlugsCannotEscapeTheirPath(t *testing.T) {
	for _, bad := range []string{"", "../etc", "a/b", "Upper", "with space", strings.Repeat("x", 100)} {
		if store.ValidWebhookSlug(bad) {
			t.Errorf("%q was accepted as a slug", bad)
		}
	}
	if !store.ValidWebhookSlug("stripe-prod") {
		t.Error("a normal slug was rejected")
	}
}

// A recipe triggers on the operator's chosen kind. That is the case the whole
// feature exists for: a workflow acts on a delivery without an agent reading it.
func TestTheChosenEventKindIsWhatGetsPublished(t *testing.T) {
	d, db := dispatcher(t)
	endpoint(t, db, store.WebhookEndpoint{Slug: "stripe", Enabled: true, EventKind: "stripe.payment"})

	if w := post(d, "stripe", `{"amount":100}`, nil); w.Code != http.StatusOK {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}

	events, err := db.RecentLogEvents(store.DefaultWorkspace, "", 20)
	if err != nil {
		t.Fatal(err)
	}
	var kinds []string
	for _, e := range events {
		kinds = append(kinds, e.Kind)
	}
	if !contains(kinds, "stripe.payment") {
		t.Errorf("the chosen kind was not published: %v", kinds)
	}
	// With no agent named, nothing extra is published: waking an agent for a
	// payload a workflow handles is the thing being avoided.
	if contains(kinds, string(bus.EventUserDefined)) {
		t.Errorf("an agent event was published with no agent configured: %v", kinds)
	}
}

// An agent only receives kinds routed at startup, and a kind invented in the
// console was not among them.
func TestNamingAnAgentAlsoPublishesSomethingItReceives(t *testing.T) {
	d, db := dispatcher(t)
	endpoint(t, db, store.WebhookEndpoint{
		Slug: "paged", Enabled: true, EventKind: "pager.alert", AgentID: "ocrew",
	})

	if w := post(d, "paged", `{"severity":"high"}`, nil); w.Code != http.StatusOK {
		t.Fatalf("%d", w.Code)
	}

	events, _ := db.RecentLogEvents(store.DefaultWorkspace, "", 20)
	var kinds []string
	for _, e := range events {
		kinds = append(kinds, e.Kind)
	}
	// Both: the chosen kind for recipes, and a routed one so the agent hears it.
	if !contains(kinds, "pager.alert") {
		t.Errorf("recipes would not fire: %v", kinds)
	}
	if !contains(kinds, string(bus.EventUserDefined)) {
		t.Errorf("the agent would never hear about it: %v", kinds)
	}
}

func contains(hay []string, needle string) bool {
	for _, h := range hay {
		if h == needle {
			return true
		}
	}
	return false
}
