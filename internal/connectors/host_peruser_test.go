package connectors

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MelloB1989/karmax/internal/store"
	"github.com/MelloB1989/karmax/pkg/connectorkit"
	"go.uber.org/zap"
)

// perUser is a minimal connector that authenticates as an individual.
type perUser struct{}

func (perUser) Manifest() connectorkit.Manifest {
	return connectorkit.Manifest{ID: "peruser", Name: "Per User", PerUser: true}
}
func (perUser) Auth() connectorkit.AuthMethod                          { return connectorkit.AuthMethod{} }
func (perUser) Tools() []connectorkit.Tool                             { return nil }
func (perUser) Sources() []connectorkit.EventSource                    { return nil }
func (perUser) Health(context.Context, connectorkit.Credentials) error { return nil }

func hostWithStore(t *testing.T) (*Host, *store.Store) {
	t.Helper()
	db, err := store.New(filepath.Join(t.TempDir(), "h.db"), zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	h := NewHost(db, nil, nil, zap.NewNop())
	h.Register(perUser{})
	if err := db.SaveCredential(store.Credential{
		Connector: "peruser", Config: map[string]string{"client_id": "cid"}, Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}
	return h, db
}

// Reading one employee's private data to answer another's question is not a
// permissions bug to tighten later. There is no safe default, so it refuses.
func TestAPerUserConnectorRefusesWithoutAnActor(t *testing.T) {
	h, _ := hostWithStore(t)

	_, err := h.credentialsFor(context.Background(), "peruser")
	if err == nil {
		t.Fatal("a per-user connector resolved credentials with nobody being acted for")
	}
	if !strings.Contains(err.Error(), "not on anyone's behalf") {
		t.Errorf("unhelpful message: %v", err)
	}
}

func TestAPerUserConnectorRefusesForSomeoneWhoHasNotConnected(t *testing.T) {
	h, _ := hostWithStore(t)

	ctx := connectorkit.WithActor(context.Background(), "priya")
	_, err := h.credentialsFor(ctx, "peruser")
	if err == nil {
		t.Fatal("resolved credentials for someone who never connected")
	}
	if !strings.Contains(err.Error(), "has not been connected by priya") {
		t.Errorf("the message does not name who needs to connect: %v", err)
	}
}

// The point of the whole design: each person's own token, chosen by who is
// being acted for.
func TestEachEmployeeGetsTheirOwnToken(t *testing.T) {
	h, db := hostWithStore(t)

	for _, m := range []string{"kartik", "priya"} {
		if err := db.SaveUserCredential(store.UserCredential{
			Connector: "peruser", Member: m, Account: m + "@acme.com",
			AccessToken: "at-" + m, RefreshToken: "rt-" + m,
		}); err != nil {
			t.Fatal(err)
		}
	}

	for _, m := range []string{"kartik", "priya"} {
		cr, err := h.credentialsFor(connectorkit.WithActor(context.Background(), m), "peruser")
		if err != nil {
			t.Fatalf("%s: %v", m, err)
		}
		if cr.AccessToken != "at-"+m {
			t.Errorf("%s got token %q — the wrong person's data would be returned", m, cr.AccessToken)
		}
		if cr.Account != m+"@acme.com" || cr.Member != m {
			t.Errorf("%s: credentials do not name the right person: %+v", m, cr)
		}
		// The org app's config must still be underneath, or a refresh has no
		// client id to present.
		if cr.Config["client_id"] != "cid" {
			t.Errorf("%s: the org app config was lost: %v", m, cr.Config)
		}
	}
}

// An install-wide connector must be unaffected by any of this.
func TestAnInstallWideConnectorIgnoresTheActor(t *testing.T) {
	db, err := store.New(filepath.Join(t.TempDir(), "h2.db"), zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	h := NewHost(db, nil, nil, zap.NewNop())
	h.Register(sharedConnector{})
	if err := db.SaveCredential(store.Credential{
		Connector: "shared", AccessToken: "one-token", Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}

	cr, err := h.credentialsFor(context.Background(), "shared")
	if err != nil || cr.AccessToken != "one-token" {
		t.Errorf("an install-wide connector broke with no actor: %v / %+v", err, cr)
	}
	cr, err = h.credentialsFor(connectorkit.WithActor(context.Background(), "kartik"), "shared")
	if err != nil || cr.AccessToken != "one-token" {
		t.Errorf("an install-wide connector changed behaviour with an actor: %v / %+v", err, cr)
	}
}

type sharedConnector struct{}

func (sharedConnector) Manifest() connectorkit.Manifest {
	return connectorkit.Manifest{ID: "shared", Name: "Shared"}
}
func (sharedConnector) Auth() connectorkit.AuthMethod                          { return connectorkit.AuthMethod{} }
func (sharedConnector) Tools() []connectorkit.Tool                             { return nil }
func (sharedConnector) Sources() []connectorkit.EventSource                    { return nil }
func (sharedConnector) Health(context.Context, connectorkit.Credentials) error { return nil }

// The actor must come from the turn, never from something the model writes.
func TestTheActorIsCarriedInContextNotArguments(t *testing.T) {
	if got := connectorkit.ActorFrom(context.Background()); got != "" {
		t.Errorf("a bare context named an actor: %q", got)
	}
	ctx := connectorkit.WithActor(context.Background(), "kartik")
	if got := connectorkit.ActorFrom(ctx); got != "kartik" {
		t.Errorf("actor did not round-trip: %q", got)
	}
	// An empty member must not overwrite a real one further up the stack.
	if got := connectorkit.ActorFrom(connectorkit.WithActor(ctx, "")); got != "kartik" {
		t.Errorf("an empty actor erased the real one: %q", got)
	}
}
