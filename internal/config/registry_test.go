package config

// The rules an operator is relying on when they narrow this list. The one that
// matters most is the first: an install that says nothing must keep everything,
// because the alternative is an upgrade silently switching somebody's
// integrations off.

import (
	"testing"

	"gopkg.in/yaml.v3"
)

func TestSayingNothingManagesEverything(t *testing.T) {
	var r RegistryConfig
	for _, id := range []string{"github", "google", "instagram", "github:work"} {
		if !r.Manages(id) {
			t.Errorf("an empty policy must manage %q — an upgrade cannot turn things off", id)
		}
	}
	if r.Restricts() {
		t.Error("an empty policy does not restrict anything")
	}
}

func TestAnExplicitlyEmptyListManagesNothing(t *testing.T) {
	// enabled: [] is not the same as omitting it. Somebody who wrote an empty
	// list meant an empty list.
	var cfg struct {
		Connectors RegistryConfig `yaml:"connectors"`
	}
	if err := yaml.Unmarshal([]byte("connectors:\n  enabled: []\n"), &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Connectors.Enabled == nil {
		t.Fatal("yaml must distinguish an empty list from an absent one, or this rule cannot exist")
	}
	if cfg.Connectors.Manages("github") {
		t.Error("an explicit empty list must manage nothing")
	}
}

func TestAnAbsentSectionParsesAsUnrestricted(t *testing.T) {
	var cfg struct {
		Connectors RegistryConfig `yaml:"connectors"`
	}
	if err := yaml.Unmarshal([]byte("agents: []\n"), &cfg); err != nil {
		t.Fatal(err)
	}
	if !cfg.Connectors.Manages("github") {
		t.Error("a config with no connectors: section must manage everything")
	}
}

func TestEnabledIsAnAllowlist(t *testing.T) {
	r := RegistryConfig{Enabled: []string{"github", "notion"}}
	if !r.Manages("github") || !r.Manages("notion") {
		t.Error("a listed connector must be managed")
	}
	if r.Manages("instagram") {
		t.Error("an unlisted connector must not be managed")
	}
}

func TestDisabledSubtractsWithoutEnumeratingTheRest(t *testing.T) {
	// "everything except instagram" must not require naming the other eleven.
	r := RegistryConfig{Disabled: []string{"instagram"}}
	if r.Manages("instagram") {
		t.Error("a disabled connector must not be managed")
	}
	if !r.Manages("github") {
		t.Error("disabling one must not disable the rest")
	}
}

func TestDisabledBeatsEnabled(t *testing.T) {
	r := RegistryConfig{Enabled: []string{"github", "instagram"}, Disabled: []string{"instagram"}}
	if r.Manages("instagram") {
		t.Error("disabled must win, or the precedence is ambiguous")
	}
	if !r.Manages("github") {
		t.Error("github was enabled and not disabled")
	}
}

func TestNamingAProviderCoversItsAccounts(t *testing.T) {
	// Somebody listing "github" means their GitHub, not just the primary login.
	r := RegistryConfig{Enabled: []string{"github"}}
	for _, id := range []string{"github", "github:work", "github:personal"} {
		if !r.Manages(id) {
			t.Errorf("listing the provider must cover %q", id)
		}
	}
	if r.Manages("gitlab") {
		t.Error("a different provider must not be swept in")
	}
}

func TestOneAccountCanBeKeptWithoutTheOther(t *testing.T) {
	r := RegistryConfig{Enabled: []string{"github:work"}}
	if !r.Manages("github:work") {
		t.Error("the named account must be managed")
	}
	if r.Manages("github:personal") {
		t.Error("an account that was not named must not be managed")
	}
	if r.Manages("github") {
		t.Error("naming one account must not enable the primary login too")
	}
}

func TestCaseAndSpacingDoNotChangeTheAnswer(t *testing.T) {
	// A hand-edited yaml file has stray spaces and inconsistent case in it.
	r := RegistryConfig{Enabled: []string{"  GitHub  ", "NOTION"}}
	if !r.Manages("github") || !r.Manages("notion") {
		t.Error("entries must match regardless of case and surrounding space")
	}
}

func TestDeclaredReportsEverythingWrittenDown(t *testing.T) {
	r := RegistryConfig{Enabled: []string{"github"}, Disabled: []string{"instagram"}}
	got := r.Declared()
	if len(got) != 2 {
		t.Fatalf("want both lists reported so typos in either can be warned about, got %v", got)
	}
}
