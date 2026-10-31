package harness

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// writeArgLinesClaude is a fake CLI that records its arguments one per line,
// so a JSON value or a path survives intact, then answers every turn.
func writeArgLinesClaude(t *testing.T) (bin, argsFile string) {
	t.Helper()
	dir := t.TempDir()
	argsFile = filepath.Join(dir, "args")
	bin = writeScript(t, "printf '%s\\n' \"$@\" > "+argsFile+"\n"+
		"while IFS= read -r line; do "+resultLine+"done\n")
	return bin, argsFile
}

// spawnedArgs opens a session of kind under cfg and returns the arguments its
// process was started with.
func spawnedArgs(t *testing.T, cfg Config, kind string) []string {
	t.Helper()
	bin, argsFile := writeArgLinesClaude(t)
	cfg.Binary, cfg.WorkdirRoot, cfg.MaxLive, cfg.Env = bin, t.TempDir(), 4, os.Environ()
	sup := New(cfg, newMemStore(), NewBreaker(0.95, nil), testLog{t}, nil)
	t.Cleanup(sup.Shutdown)
	if _, err := sup.Send(context.Background(), "key", kind, "hi"); err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSuffix(readArgs(t, argsFile), "\n"), "\n")
}

// flagValues is every value given to flag.
func flagValues(args []string, flag string) []string {
	var out []string
	for i, a := range args {
		if a == flag && i+1 < len(args) {
			out = append(out, args[i+1])
		}
	}
	return out
}

// settingsOf decodes the --settings a session was given; nil when none.
func settingsOf(t *testing.T, args []string) map[string]any {
	t.Helper()
	vals := flagValues(args, "--settings")
	if len(vals) == 0 {
		return nil
	}
	if len(vals) > 1 {
		t.Fatalf("--settings given %d times: %q", len(vals), vals)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(vals[0]), &m); err != nil {
		t.Fatalf("--settings is not JSON: %v (%q)", err, vals[0])
	}
	return m
}

// skillsIn lists the skills a session's --add-dir hands the CLI, by the name
// the CLI will give them, each mapped to where it really lives.
func skillsIn(t *testing.T, args []string) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, dir := range flagValues(args, "--add-dir") {
		ents, err := os.ReadDir(filepath.Join(dir, ".claude", "skills"))
		if err != nil {
			t.Fatalf("--add-dir %s holds no skills: %v", dir, err)
		}
		for _, e := range ents {
			target, err := filepath.EvalSymlinks(filepath.Join(dir, ".claude", "skills", e.Name()))
			if err != nil {
				t.Fatalf("skill %s: %v", e.Name(), err)
			}
			out[e.Name()] = target
		}
	}
	return out
}

func skillDir(t *testing.T, root, name string) string {
	t.Helper()
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("---\nname: "+name+"\n---\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	real, _ := filepath.EvalSymlinks(dir)
	return real
}

// A session loads none of the operator's own Claude Code config: not their
// plugins (one injected a start-up prompt into every session, another ran its
// hooks on every shell command), not their skills, not their hooks. It still
// loads its workdir's project config, which is where CLAUDE.md comes from.
func TestASessionLoadsNoneOfTheOperatorsOwnClaudeConfig(t *testing.T) {
	args := spawnedArgs(t, Config{Policies: map[string]Policy{"k": {TurnTimeout: 5 * time.Second}}}, "k")
	if got := flagValues(args, "--setting-sources"); !slices.Equal(got, []string{"project,local"}) {
		t.Errorf("--setting-sources = %q, want [project,local]: %q", got, args)
	}
	for _, flag := range []string{"--settings", "--plugin-dir", "--add-dir"} {
		if slices.Contains(args, flag) {
			t.Errorf("nothing was allowlisted, yet the session got %s: %q", flag, args)
		}
	}
}

// A kind that opts back in loads the operator's whole Claude Code config, as
// every session once did.
func TestAKindThatInheritsTheUserConfigLoadsIt(t *testing.T) {
	args := spawnedArgs(t, Config{Policies: map[string]Policy{
		"k": {TurnTimeout: 5 * time.Second, InheritUserConfig: true},
	}}, "k")
	if slices.Contains(args, "--setting-sources") {
		t.Errorf("a kind that inherits the user config was limited: %q", args)
	}
}

// What the operator allowlists reaches every session: an installed plugin by
// its id, a plugin by its directory, a skill by its directory, and settings.
func TestAllowlistedPluginsSkillsAndSettingsReachTheSession(t *testing.T) {
	skills := t.TempDir()
	fleet := skillDir(t, skills, "fleet")
	args := spawnedArgs(t, Config{
		Plugins:  []string{"helper@acme-plugins", "/opt/acme/plugins/linter"},
		Skills:   []string{fleet},
		Settings: map[string]any{"effortLevel": "high"},
		Policies: map[string]Policy{"k": {TurnTimeout: 5 * time.Second}},
	}, "k")

	set := settingsOf(t, args)
	if set["effortLevel"] != "high" {
		t.Errorf("settings = %v, want the configured effortLevel", set)
	}
	if enabled, _ := set["enabledPlugins"].(map[string]any); enabled["helper@acme-plugins"] != true || len(enabled) != 1 {
		t.Errorf("enabledPlugins = %v, want only helper@acme-plugins", set["enabledPlugins"])
	}
	if got := flagValues(args, "--plugin-dir"); !slices.Equal(got, []string{"/opt/acme/plugins/linter"}) {
		t.Errorf("--plugin-dir = %q", got)
	}
	// Under its own name, not a plugin's namespace: the brief says "fleet".
	if got := skillsIn(t, args); len(got) != 1 || got["fleet"] != fleet {
		t.Errorf("skills = %v, want fleet → %s", got, fleet)
	}
	// Still limited: the allowlist adds to nothing, not to the operator's set.
	if got := flagValues(args, "--setting-sources"); !slices.Equal(got, []string{"project,local"}) {
		t.Errorf("--setting-sources = %q", got)
	}
}

// A kind's own lists replace the defaults, an empty one included; a kind that
// names none keeps them.
func TestAKindsOwnListsReplaceTheDefaults(t *testing.T) {
	skills := t.TempDir()
	fleet, notes := skillDir(t, skills, "fleet"), skillDir(t, skills, "notes")
	cfg := Config{
		Plugins: []string{"helper@acme-plugins"},
		Skills:  []string{notes},
		Policies: map[string]Policy{
			"plain":        {TurnTimeout: 5 * time.Second},
			"orchestrator": {TurnTimeout: 5 * time.Second, Skills: []string{fleet, notes}},
			"bare":         {TurnTimeout: 5 * time.Second, Plugins: []string{}, Skills: []string{}},
		},
	}

	plain := spawnedArgs(t, cfg, "plain")
	if got := skillsIn(t, plain); len(got) != 1 || got["notes"] != notes {
		t.Errorf("plain skills = %v, want the default (notes)", got)
	}

	orch := spawnedArgs(t, cfg, "orchestrator")
	if got := skillsIn(t, orch); len(got) != 2 || got["fleet"] != fleet || got["notes"] != notes {
		t.Errorf("orchestrator skills = %v, want fleet and notes", got)
	}
	if enabled, _ := settingsOf(t, orch)["enabledPlugins"].(map[string]any); enabled["helper@acme-plugins"] != true {
		t.Errorf("orchestrator named no plugins of its own, so it keeps the default: %q", orch)
	}

	bare := spawnedArgs(t, cfg, "bare")
	if slices.Contains(bare, "--add-dir") || slices.Contains(bare, "--settings") {
		t.Errorf("a kind with empty lists still got the defaults: %q", bare)
	}
}

// A variadic flag reads every argument up to the next flag, so whatever
// follows its value must be a flag: a path or a prompt after --add-dir would
// be taken for another directory.
func TestVariadicFlagsAreFollowedByAFlag(t *testing.T) {
	skills := t.TempDir()
	args := spawnArgs(&Session{ID: "s", SkillsDir: skills, PluginDirs: []string{"/p"},
		Settings: `{"a":1}`, OnlyMCPConfig: `{"mcpServers":{}}`, MCPConfig: `{"mcpServers":{}}`,
		SystemPrompt: "be brief"}, false, "")
	for i, a := range args {
		if a != "--add-dir" && a != "--mcp-config" {
			continue
		}
		if i+2 < len(args) && !strings.HasPrefix(args[i+2], "--") {
			t.Errorf("%s %s is followed by %q, which it would read as one more value: %q", a, args[i+1], args[i+2], args)
		}
	}
}

// The same set of skills, opened again and again, links into the same place
// and survives being gathered by two sessions at once.
func TestGatheringSkillsIsStableAndSafeToRepeat(t *testing.T) {
	root, skills := t.TempDir(), t.TempDir()
	set := []string{skillDir(t, skills, "fleet"), skillDir(t, skills, "notes")}
	first, missing, err := gatherSkills(root, set)
	if err != nil || len(missing) != 0 {
		t.Fatal(err, missing)
	}
	done := make(chan string, 8)
	for range 8 {
		go func() {
			dir, _, err := gatherSkills(root, set)
			if err != nil {
				t.Error(err)
			}
			done <- dir
		}()
	}
	for range 8 {
		if dir := <-done; dir != first {
			t.Errorf("the same skills gathered into %s and %s", first, dir)
		}
	}
	ents, _ := os.ReadDir(filepath.Join(first, ".claude", "skills"))
	if len(ents) != 2 {
		t.Errorf("gathered %d entries, want 2 (stray temporary links?)", len(ents))
	}
	if other, _, _ := gatherSkills(root, set[:1]); other == first {
		t.Error("two different sets of skills share one directory")
	}
}

// A skill that is not there is reported, not linked: a link to nothing would
// hide the mistake.
func TestAMissingSkillIsReported(t *testing.T) {
	root := t.TempDir()
	dir, missing, err := gatherSkills(root, []string{filepath.Join(root, "no-such-skill")})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(missing, []string{filepath.Join(root, "no-such-skill")}) {
		t.Errorf("missing = %q", missing)
	}
	if ents, _ := os.ReadDir(filepath.Join(dir, ".claude", "skills")); len(ents) != 0 {
		t.Errorf("a missing skill was linked: %v", ents)
	}
}
