package fsscope

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func settings(t *testing.T, p Policy, dataDir string) Permissions {
	t.Helper()
	raw := p.SettingsJSON(dataDir)
	if raw == "" {
		t.Fatal("no settings rendered")
	}
	var s Settings
	if err := json.Unmarshal([]byte(raw), &s); err != nil {
		t.Fatalf("settings are not valid JSON: %v", err)
	}
	return s.Permissions
}

// No policy means the harness runs exactly as it did before this existed.
//
// The caller reads "" as "pass --dangerously-skip-permissions", so a stray
// non-empty answer here would silently switch every install to a permission
// mode nobody asked for.
func TestNoPolicyRendersNothing(t *testing.T) {
	if got := (Policy{}).SettingsJSON(t.TempDir()); got != "" {
		t.Fatalf("SettingsJSON = %q, want empty for an unenforced policy", got)
	}
}

// The rule syntax that took a live test to find, pinned so it cannot drift.
//
// Read(/x/**) is a path relative to the project and matches nothing;
// Read(//x/**) is the absolute one. Both look like a working rule in a diff.
func TestAbsoluteRulesCarryTwoSlashes(t *testing.T) {
	p := Policy{Enforced: true, Deny: []string{"/tmp/karmax-test-secrets"}}
	perms := settings(t, p, t.TempDir())
	want := "Read(//tmp/karmax-test-secrets/**)"
	if !has(perms.Deny, want) {
		t.Fatalf("missing %q in %v", want, perms.Deny)
	}
	for _, rule := range perms.Deny {
		if strings.HasPrefix(rule, "Read(/") && !strings.HasPrefix(rule, "Read(//") {
			t.Errorf("rule with one leading slash matches nothing: %s", rule)
		}
	}
}

// A denied file needs the bare path as well as the /** form.
//
// ~/.claude.json is a file. The /** form alone would match nothing while
// looking exactly like a rule that works.
func TestDeniedFilesGetABarePathRuleToo(t *testing.T) {
	p := Policy{Enforced: true, Deny: []string{"/tmp/karmax-test-secrets/token.json"}}
	perms := settings(t, p, t.TempDir())
	for _, want := range []string{
		"Read(//tmp/karmax-test-secrets/token.json)",
		"Read(//tmp/karmax-test-secrets/token.json/**)",
	} {
		if !has(perms.Deny, want) {
			t.Errorf("missing %q", want)
		}
	}
}

// Writes are denied with Edit, never Write.
//
// Claude Code ignores Write(path) rules — it warns on stderr and carries on,
// which means a rule written that way is a block that never blocks.
func TestWriteRulesAreEditRules(t *testing.T) {
	p := Policy{Enforced: true, Deny: []string{"/tmp/karmax-test-secrets"}}
	perms := settings(t, p, t.TempDir())
	if !has(perms.Deny, "Edit(//tmp/karmax-test-secrets/**)") {
		t.Fatalf("no Edit rule in %v", perms.Deny)
	}
	for _, rule := range perms.Deny {
		if strings.HasPrefix(rule, "Write(") {
			t.Errorf("Write() rule matches nothing: %s", rule)
		}
	}
}

// A read-only grant blocks edits there. This is the only half of a grant that
// is actually enforced, so it had better be.
func TestReadOnlyGrantBlocksEdits(t *testing.T) {
	dir := t.TempDir()
	var p Policy
	if err := p.Allow(dir, false); err != nil {
		t.Fatal(err)
	}
	perms := settings(t, p, t.TempDir())
	if !hasPrefixIn(perms.Deny, "Edit(//") {
		t.Fatalf("a read-only grant produced no Edit denial: %v", perms.Deny)
	}
	for _, rule := range perms.Deny {
		if strings.HasPrefix(rule, "Read(") && strings.Contains(rule, dir) {
			t.Errorf("a read-only grant denied reading it: %s", rule)
		}
	}
}

func TestWritableGrantIsNotDenied(t *testing.T) {
	dir := t.TempDir()
	var p Policy
	if err := p.Allow(dir, true); err != nil {
		t.Fatal(err)
	}
	perms := settings(t, p, t.TempDir())
	for _, rule := range perms.Deny {
		if strings.Contains(rule, dir) {
			t.Errorf("a writable grant was denied: %s", rule)
		}
	}
}

// The allow list is what stops a policy about FILES from taking the web away.
//
// dontAsk denies anything not named, so an empty or partial allow list would
// turn "restrict my Documents folder" into "the agent can no longer search the
// web", which is not a trade anybody agreed to.
func TestPolicyDoesNotTakeAwayTheRestOfTheToolbox(t *testing.T) {
	p := Policy{Enforced: true}
	perms := settings(t, p, t.TempDir())
	for _, tool := range []string{"Bash", "WebSearch", "WebFetch", "Read", "Edit", "Glob", "Grep", "Task"} {
		if !has(perms.Allow, tool) {
			t.Errorf("%s is not allowed; the policy would remove it", tool)
		}
	}
}

// The browser profile is never readable.
//
// "The browser being open is the grant" only holds while the agent cannot read
// the cookie jar. One Read there turns a window somebody can close into every
// account they have signed into, permanently.
func TestTheBrowserProfileIsAlwaysOutOfReach(t *testing.T) {
	data := t.TempDir()
	perms := settings(t, Policy{Enforced: true}, data)
	profile := filepath.Join(data, "browser")
	found := false
	for _, rule := range perms.Deny {
		if strings.HasPrefix(rule, "Read(") && strings.Contains(rule, profile) {
			found = true
		}
	}
	if !found {
		t.Fatalf("the browser profile %s is readable", profile)
	}
}

// Credential stores are denied even when the operator has restricted nothing.
func TestStandardDeniesCoverTheObviousSecrets(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home directory")
	}
	list := StandardDenies(t.TempDir())
	for _, want := range []string{".ssh", ".aws", ".gnupg"} {
		if !has(list, filepath.Join(home, want)) {
			t.Errorf("~/%s is not in the standard denials", want)
		}
	}
}

// Granting a folder turns enforcement on, because the operator has now said
// something. Doing it silently either way is the surprise.
func TestSayingAnythingTurnsEnforcementOn(t *testing.T) {
	var p Policy
	if p.Enforced {
		t.Fatal("a fresh policy is enforced")
	}
	if err := p.Allow(t.TempDir(), true); err != nil {
		t.Fatal(err)
	}
	if !p.Enforced {
		t.Fatal("granting a folder did not turn enforcement on")
	}
}

// A grant that is not there is a typo, and the time to say so is now.
func TestGrantingSomethingThatIsNotThereFails(t *testing.T) {
	var p Policy
	if err := p.Allow(filepath.Join(t.TempDir(), "nope"), true); err == nil {
		t.Fatal("granted a directory that does not exist")
	}
	file := filepath.Join(t.TempDir(), "a.txt")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := p.Allow(file, true); err == nil {
		t.Fatal("granted a file as though it were a folder")
	}
}

// Round-tripping through disk keeps every field.
func TestSaveAndLoad(t *testing.T) {
	dir := t.TempDir()
	granted := t.TempDir()
	var p Policy
	if err := p.Allow(granted, true); err != nil {
		t.Fatal(err)
	}
	if err := p.Forbid(dir); err != nil {
		t.Fatal(err)
	}
	if err := Save(dir, p); err != nil {
		t.Fatal(err)
	}
	back := Load(dir)
	if !back.Enforced || len(back.Grants) != 1 || len(back.Deny) != 1 {
		t.Fatalf("round trip lost something: %+v", back)
	}
	if !back.Grants[0].Write {
		t.Error("the write bit did not survive")
	}
}

// The standard list cannot be removed, only added to.
func TestStandardDenialsCannotBeUnforbidden(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home directory")
	}
	var p Policy
	if err := p.Unforbid(filepath.Join(home, ".ssh")); err == nil {
		t.Fatal("removed a standard denial")
	}
}

func has(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

func hasPrefixIn(list []string, prefix string) bool {
	for _, s := range list {
		if strings.HasPrefix(s, prefix) {
			return true
		}
	}
	return false
}
