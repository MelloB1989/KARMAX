package connectors

import (
	"context"
	"errors"
	"testing"

	"github.com/MelloB1989/karmax/pkg/connectorkit"
)

// A connector whose Health can be made to pass, fail, or panic.
type probeConnector struct {
	id      string
	err     error
	panics  bool
	perUser bool
}

func (p probeConnector) Manifest() connectorkit.Manifest {
	return connectorkit.Manifest{ID: p.id, Name: p.id, PerUser: p.perUser}
}
func (p probeConnector) Auth() connectorkit.AuthMethod       { return connectorkit.AuthMethod{} }
func (p probeConnector) Tools() []connectorkit.Tool          { return nil }
func (p probeConnector) Sources() []connectorkit.EventSource { return nil }
func (p probeConnector) Health(context.Context, connectorkit.Credentials) error {
	if p.panics {
		panic("the vendor's client exploded")
	}
	return p.err
}

// The whole point: a verdict has to survive a restart. It used to live in a map
// on the console server, so every boot reported everything as unchecked.
func TestAVerdictIsPersisted(t *testing.T) {
	h, db := hostWithStore(t)
	h.Register(probeConnector{id: "good"})
	if err := db.SaveCredential(storeCredential("good")); err != nil {
		t.Fatal(err)
	}

	got := h.Probe(context.Background(), "good")
	if got.Status != "healthy" {
		t.Fatalf("expected healthy, got %s / %s", got.Status, got.Detail)
	}

	stored, err := db.ConnectorHealthFor("good")
	if err != nil || stored == nil {
		t.Fatalf("the verdict was not written: %v", err)
	}
	if stored.Status != "healthy" || stored.CheckedAt.IsZero() {
		t.Errorf("stored verdict is wrong: %+v", stored)
	}
}

func TestAFailureIsRecordedWithItsReason(t *testing.T) {
	h, db := hostWithStore(t)
	h.Register(probeConnector{id: "bad", err: errors.New("token was rejected")})
	if err := db.SaveCredential(storeCredential("bad")); err != nil {
		t.Fatal(err)
	}

	got := h.Probe(context.Background(), "bad")
	if got.Status != "failed" {
		t.Errorf("expected failed, got %s", got.Status)
	}
	if got.Detail != "token was rejected" {
		t.Errorf("the reason was lost: %q", got.Detail)
	}
}

// The prober runs on a background goroutine. A panic inside a vendor's client
// would otherwise take the whole daemon down — an integration being broken must
// not stop the agent answering.
func TestAPanickingConnectorDoesNotTakeTheProcessDown(t *testing.T) {
	h, db := hostWithStore(t)
	h.Register(probeConnector{id: "explodes", panics: true})
	if err := db.SaveCredential(storeCredential("explodes")); err != nil {
		t.Fatal(err)
	}

	got := h.Probe(context.Background(), "explodes")
	if got.Status != "failed" {
		t.Errorf("a panic should be recorded as failed, got %s", got.Status)
	}
	if got.Detail == "" {
		t.Error("a panic produced no explanation")
	}

	// And the sweep keeps going rather than stopping at the bad one.
	h.Register(probeConnector{id: "fine"})
	if err := db.SaveCredential(storeCredential("fine")); err != nil {
		t.Fatal(err)
	}
	h.ProbeAll(context.Background())
	if v, _ := db.ConnectorHealthFor("fine"); v == nil || v.Status != "healthy" {
		t.Error("the sweep stopped at the connector that panicked")
	}
}

// Checking a per-user connector as the install would either fail, or worse
// succeed using whichever employee's token happened to be stored first.
func TestAPerUserConnectorIsNotProbedAsTheInstall(t *testing.T) {
	h, db := hostWithStore(t)
	h.Register(probeConnector{id: "peruser2", perUser: true, err: errors.New("must not be called")})

	// No org app configured yet.
	if got := h.Probe(context.Background(), "peruser2"); got.Status != "not_configured" {
		t.Errorf("expected not_configured, got %s / %s", got.Status, got.Detail)
	}

	// App configured, nobody connected.
	if err := db.SaveCredential(storeCredential("peruser2")); err != nil {
		t.Fatal(err)
	}
	got := h.Probe(context.Background(), "peruser2")
	if got.Status != "degraded" {
		t.Errorf("expected degraded, got %s", got.Status)
	}

	// Somebody connected: healthy, and counted — without ever calling Health,
	// which would have returned the error above.
	if err := db.SaveUserCredential(userCred("peruser2", "kartik")); err != nil {
		t.Fatal(err)
	}
	got = h.Probe(context.Background(), "peruser2")
	if got.Status != "healthy" || got.Detail != "1 person connected" {
		t.Errorf("got %s / %q", got.Status, got.Detail)
	}
}

func TestAnUnconfiguredConnectorIsNotCalled(t *testing.T) {
	h, _ := hostWithStore(t)
	h.Register(probeConnector{id: "nocreds", err: errors.New("must not be called")})

	got := h.Probe(context.Background(), "nocreds")
	if got.Status != "not_configured" {
		t.Errorf("expected not_configured, got %s / %s", got.Status, got.Detail)
	}
}
