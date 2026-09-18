package connectors

// Proving the policy reaches the registry, not just the matcher.
//
// The unit tests in internal/config cover what Manages() decides; these cover
// that Register actually obeys it, and that a name matching nothing is
// reported rather than silently doing nothing.

import (
	"context"
	"encoding/json"
	"slices"
	"testing"

	"github.com/MelloB1989/karmax/internal/config"
	"github.com/MelloB1989/karmax/pkg/connectorkit"
	"go.uber.org/zap"
)

// fake is the smallest thing the registry will accept.
type fake struct{ id string }

func (f fake) Manifest() connectorkit.Manifest {
	return connectorkit.Manifest{ID: f.id, Name: f.id}
}
func (f fake) Auth() connectorkit.AuthMethod { return connectorkit.AuthMethod{} }
func (f fake) Tools() []connectorkit.Tool    { return nil }
func (f fake) Sources() []connectorkit.EventSource {
	return nil
}
func (f fake) Health(context.Context, connectorkit.Credentials) error { return nil }

var _ = json.Marshal // keep the import honest if Tools() grows

func hostWith(policy config.RegistryConfig, ids ...string) *Host {
	h := &Host{registry: map[string]connectorkit.Connector{}, log: zap.NewNop()}
	h.Manage(policy)
	for _, id := range ids {
		h.Register(fake{id: id})
	}
	return h
}

func availableIDs(h *Host) []string {
	out := []string{}
	for _, m := range h.Available() {
		out = append(out, m.ID)
	}
	slices.Sort(out)
	return out
}

func TestWithNoPolicyEverythingRegisters(t *testing.T) {
	h := hostWith(config.RegistryConfig{}, "github", "notion", "instagram")
	if got := availableIDs(h); len(got) != 3 {
		t.Errorf("an install that says nothing keeps everything, got %v", got)
	}
}

func TestAnAllowlistKeepsOnlyWhatItNames(t *testing.T) {
	h := hostWith(config.RegistryConfig{Enabled: []string{"github", "notion"}},
		"github", "notion", "instagram", "slack")
	got := availableIDs(h)
	want := []string{"github", "notion"}
	if !slices.Equal(got, want) {
		t.Errorf("want %v, got %v", want, got)
	}
}

func TestADisabledConnectorIsNeverRegistered(t *testing.T) {
	// Not merely hidden: an unregistered connector has no tools, so an agent
	// cannot be talked into reaching for it either.
	h := hostWith(config.RegistryConfig{Disabled: []string{"instagram"}},
		"github", "instagram")
	if slices.Contains(availableIDs(h), "instagram") {
		t.Error("a disabled connector must not be in the registry at all")
	}
	if _, ok := h.registry["instagram"]; ok {
		t.Error("it must not be reachable by id either")
	}
}

func TestNamingAProviderKeepsItsAccounts(t *testing.T) {
	h := hostWith(config.RegistryConfig{Enabled: []string{"github"}},
		"github", "github:work", "notion")
	got := availableIDs(h)
	want := []string{"github", "github:work"}
	if !slices.Equal(got, want) {
		t.Errorf("want %v, got %v", want, got)
	}
}

func TestATypoIsReportedRatherThanSilentlyMatchingNothing(t *testing.T) {
	h := hostWith(config.RegistryConfig{Enabled: []string{"github", "githbu"}},
		"github", "notion")
	unknown := h.UnknownDeclared()
	if !slices.Contains(unknown, "githbu") {
		t.Errorf("the typo must be reported, got %v", unknown)
	}
	if slices.Contains(unknown, "github") {
		t.Errorf("a name that matched must not be reported, got %v", unknown)
	}
}

func TestAnAccountQualifiedNameIsNotMistakenForATypo(t *testing.T) {
	h := hostWith(config.RegistryConfig{Enabled: []string{"github"}}, "github:work")
	if unknown := h.UnknownDeclared(); len(unknown) != 0 {
		t.Errorf("listing the provider of a registered account is not a typo, got %v", unknown)
	}
}

func TestNothingIsReportedWhenNoPolicyWasWritten(t *testing.T) {
	h := hostWith(config.RegistryConfig{}, "github")
	if unknown := h.UnknownDeclared(); len(unknown) != 0 {
		t.Errorf("an install with no policy has nothing to warn about, got %v", unknown)
	}
}
