package connectors

import (
	"context"
	"net/http"
	"testing"

	"github.com/MelloB1989/karmax/pkg/connectorkit"
)

// A connector whose webhook path collides with a console SPA route — which is
// the normal case, since connectors name their paths for readability.
type hookedConnector struct{}

func (hookedConnector) Manifest() connectorkit.Manifest {
	return connectorkit.Manifest{ID: "hooked", Name: "Hooked"}
}
func (hookedConnector) Auth() connectorkit.AuthMethod { return connectorkit.AuthMethod{} }
func (hookedConnector) Tools() []connectorkit.Tool    { return nil }
func (hookedConnector) Sources() []connectorkit.EventSource {
	return []connectorkit.EventSource{{
		ID: "webhook", Kind: connectorkit.SourceWebhook,
		EventKind: "hooked.event", Path: "/connectors/hooked",
	}}
}
func (hookedConnector) Health(context.Context, connectorkit.Credentials) error { return nil }

// The connector's own path collides with the console page at /connectors/:id,
// so a CDN in front of both cannot tell a browser opening that page from a
// third party POSTing to it. Mounting under /hooks as well is what lets one
// HTTPS front door serve both — and keeping the original means an install that
// already told GitHub the old address does not break.
func TestWebhooksMountUnderBothPaths(t *testing.T) {
	h, db := hostWithStore(t)
	_ = db
	h.Register(hookedConnector{})
	if err := h.store.SaveCredential(storeCredential("hooked")); err != nil {
		t.Fatal(err)
	}

	mounted := map[string]bool{}
	h.MountWebhooks(func(pattern string, _ http.HandlerFunc) { mounted[pattern] = true })

	if !mounted["/connectors/hooked"] {
		t.Error("the connector's own path is no longer mounted — existing installs would break")
	}
	if !mounted["/hooks/connectors/hooked"] {
		t.Error("the /hooks path is not mounted, so no CDN can route to it without colliding")
	}
	if len(mounted) != 2 {
		t.Errorf("expected exactly two mounts, got %v", mounted)
	}
}

// Only connectors with credentials get mounted, which is why a delivery test
// before a restart 404s.
func TestWebhooksMountOnlyForConfiguredConnectors(t *testing.T) {
	h, _ := hostWithStore(t)
	h.Register(hookedConnector{}) // no credentials saved

	mounted := map[string]bool{}
	h.MountWebhooks(func(pattern string, _ http.HandlerFunc) { mounted[pattern] = true })

	if len(mounted) != 0 {
		t.Errorf("an unconfigured connector mounted webhooks: %v", mounted)
	}
}
