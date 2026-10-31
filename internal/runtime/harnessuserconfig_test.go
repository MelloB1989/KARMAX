package runtime

import (
	"path/filepath"
	"slices"
	"testing"

	"github.com/MelloB1989/karmax/internal/config"
	"gopkg.in/yaml.v3"
)

// A skill is named as the operator knows it — the name of one of their own
// skills — or given by its directory.
func TestHarnessSkillsResolveByNameOrDirectory(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	got := harnessSkillDirs([]string{"fleet", "/opt/skills/notes", "~/work/skills/review", " "})
	want := []string{
		filepath.Join(home, ".claude", "skills", "fleet"),
		"/opt/skills/notes",
		filepath.Join(home, "work", "skills", "review"),
	}
	if !slices.Equal(got, want) {
		t.Errorf("skills = %q, want %q", got, want)
	}

	// Claude Code's own config directory moves with CLAUDE_CONFIG_DIR, and
	// the operator's skills with it.
	cfgDir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfgDir)
	if got := harnessSkillDirs([]string{"fleet"}); !slices.Equal(got, []string{filepath.Join(cfgDir, "skills", "fleet")}) {
		t.Errorf("under CLAUDE_CONFIG_DIR: %q", got)
	}
}

// A plugin's id passes as written; a directory has ~ expanded.
func TestHarnessPluginsKeepIDsAndExpandDirectories(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	got := harnessPluginRefs([]string{"helper@acme-plugins", "~/plugins/linter", "/opt/plugins/x"})
	want := []string{"helper@acme-plugins", filepath.Join(home, "plugins", "linter"), "/opt/plugins/x"}
	if !slices.Equal(got, want) {
		t.Errorf("plugins = %q, want %q", got, want)
	}
}

// A kind's own plugins and skills reach its policy; a kind that names none
// keeps the defaults (nil), and one that names an empty list gets none.
func TestHarnessPoliciesCarryEachKindsPluginsAndSkills(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	var hc config.HarnessConfig
	src := `
plugins: [helper@acme-plugins]
skills: [notes]
settings:
  effortLevel: high
kinds:
  orchestrator:
    model: sonnet
    skills: [fleet]
  bare:
    plugins: []
    skills: []
  task:
    inherit_user_config: true
  chat:
    model: sonnet
`
	if err := yaml.Unmarshal([]byte(src), &hc); err != nil {
		t.Fatal(err)
	}
	pols := harnessPolicies(hc)
	if got := pols["orchestrator"].Skills; !slices.Equal(got, []string{filepath.Join(home, ".claude", "skills", "fleet")}) {
		t.Errorf("orchestrator skills = %q", got)
	}
	if pols["orchestrator"].Plugins != nil {
		t.Errorf("orchestrator named no plugins, so it keeps the default; got %q", pols["orchestrator"].Plugins)
	}
	if b := pols["bare"]; b.Plugins == nil || len(b.Plugins) != 0 || b.Skills == nil || len(b.Skills) != 0 {
		t.Errorf("bare asked for none: plugins %#v skills %#v", b.Plugins, b.Skills)
	}
	if !pols["task"].InheritUserConfig || pols["chat"].InheritUserConfig {
		t.Errorf("inherit_user_config: task %v, chat %v", pols["task"].InheritUserConfig, pols["chat"].InheritUserConfig)
	}
	if c := pols["chat"]; c.Plugins != nil || c.Skills != nil {
		t.Errorf("chat named nothing, so it keeps the defaults: %+v", c)
	}
	if hc.Settings["effortLevel"] != "high" {
		t.Errorf("settings = %v", hc.Settings)
	}
}
