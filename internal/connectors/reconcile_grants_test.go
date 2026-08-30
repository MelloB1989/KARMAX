package connectors

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/MelloB1989/karmax/internal/broker"
	"github.com/MelloB1989/karmax/internal/store"
	"github.com/MelloB1989/karmax/pkg/connectorkit"
	"go.uber.org/zap"
)

// granted is a connector with something to grant.
type granted struct{}

func (granted) Manifest() connectorkit.Manifest {
	return connectorkit.Manifest{
		ID: "granted", Name: "Granted",
		Capabilities: []string{"http:googleapis.com"},
	}
}
func (granted) Auth() connectorkit.AuthMethod       { return connectorkit.AuthMethod{} }
func (granted) Sources() []connectorkit.EventSource { return nil }
func (granted) Tools() []connectorkit.Tool {
	return []connectorkit.Tool{{Name: "granted.mail.search"}, {Name: "granted.mail.read"}}
}
func (granted) Health(context.Context, connectorkit.Credentials) error { return nil }

// Grants were written only when credentials were saved through the console. A
// connector enabled another way — an OAuth sign-in, a row predating the code
// that grants — was enabled, showed the agent its tools, and had no grant
// behind them. Every call was then refused, which reads as a broken connector
// rather than a missing grant.
func TestBootRepairsAnEnabledConnectorWithNoGrants(t *testing.T) {
	db, err := store.New(filepath.Join(t.TempDir(), "h.db"), zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	brk := broker.New(db, zap.NewNop())
	h := NewHost(db, nil, brk, zap.NewNop())
	h.Register(granted{})

	// Enabled, exactly as an OAuth connect leaves it: no grants anywhere.
	if err := db.SaveCredential(store.Credential{Connector: "granted", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	subject := broker.ConnectorSubject("granted")
	if grants, _ := db.Grants(subject); len(grants) != 0 {
		t.Fatalf("expected no grants before reconciling, got %d", len(grants))
	}

	h.ReconcileGrants()

	grants, err := db.Grants(subject)
	if err != nil {
		t.Fatal(err)
	}
	have := map[string]bool{}
	for _, g := range grants {
		have[g.Capability+":"+g.Value] = true
	}
	// Its own tools, and the host it must reach to run them.
	for _, want := range []string{
		string(store.CapTool) + ":granted.mail.search",
		string(store.CapTool) + ":granted.mail.read",
		"http:googleapis.com",
	} {
		if !have[want] {
			t.Errorf("missing %q after reconcile — the Broker still refuses that call", want)
		}
	}
}

// A connector nobody enabled must not quietly gain capabilities at boot.
func TestBootDoesNotGrantADisabledConnector(t *testing.T) {
	db, err := store.New(filepath.Join(t.TempDir(), "h.db"), zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	brk := broker.New(db, zap.NewNop())
	h := NewHost(db, nil, brk, zap.NewNop())
	h.Register(granted{})
	if err := db.SaveCredential(store.Credential{Connector: "granted", Enabled: false}); err != nil {
		t.Fatal(err)
	}

	h.ReconcileGrants()

	if grants, _ := db.Grants(broker.ConnectorSubject("granted")); len(grants) != 0 {
		t.Errorf("a disabled connector was granted %d capabilities at boot", len(grants))
	}
}
