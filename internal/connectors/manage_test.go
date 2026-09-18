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

// A connector RegisterUnconditional calls is not always one Manage() allows,
// and the earlier version of this got that wrong: Register quietly skipped
// the id, RegisterUnconditional carried on regardless, and GrantFromManifest
// then failed loudly trying to grant tools for a connector that was never
// added — an ERROR log for something that was working exactly as configured.
// Caught by actually running a build against a real narrowed config, not by
// a unit test, which is why this one exists now.
func TestRegisterUnconditionalRespectsThePolicyToo(t *testing.T) {
	h := &Host{registry: map[string]connectorkit.Connector{}, log: zap.NewNop()}
	h.Manage(config.RegistryConfig{Enabled: []string{"x"}})

	h.RegisterUnconditional(fake{id: "x"})
	h.RegisterUnconditional(fake{id: "linkedin"})

	if _, ok := h.registry["linkedin"]; ok {
		t.Fatal("a connector the policy excludes must not be registered by RegisterUnconditional either")
	}
	if !h.unconditional["x"] {
		t.Error("the allowed connector must still be marked unconditional")
	}
	if h.unconditional["linkedin"] {
		t.Error("the excluded connector must not be marked unconditional")
	}
}
