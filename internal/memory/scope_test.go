package memory

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MelloB1989/karmax/internal/store"
	"github.com/MelloB1989/karmax/pkg/connectorkit"
	"go.uber.org/zap"
)

func scopes(t *testing.T) (*Scopes, *store.Store) {
	t.Helper()
	dir := t.TempDir()
	db, err := store.New(filepath.Join(dir, "s.db"), zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	f := NewFactory(dir, db, zap.NewNop())
	t.Cleanup(f.StopAll)
	return NewScopes(f, db, "ocrew", "acme", zap.NewNop()), db
}

// Work that is nobody's request — a cron tick, a webhook, a recipe — sees the
// shared namespace and nothing else. That is the whole point of separating them.
func TestWorkOnNobodysBehalfSeesOnlyGlobal(t *testing.T) {
	s, _ := scopes(t)
	ctx := context.Background()

	read := s.Read(ctx)
	if len(read) != 1 {
		t.Fatalf("expected only the global namespace, got %d", len(read))
	}
	if read[0].Namespace() != "acme" {
		t.Errorf("read from %q, want acme", read[0].Namespace())
	}
	if got := s.Write(ctx).Namespace(); got != "acme" {
		t.Errorf("wrote to %q, want acme", got)
	}
}

// What the agent learns while helping somebody is theirs until somebody says
// otherwise.
func TestActingForAMemberWritesToTheirNamespace(t *testing.T) {
	s, _ := scopes(t)
	ctx := connectorkit.WithActor(context.Background(), "priya")

	if got := s.Write(ctx).Namespace(); got != "acme-member-priya" {
		t.Errorf("wrote to %q — a private remark must not land in the shared namespace", got)
	}

	// And reads see both, global first.
	read := s.Read(ctx)
	if len(read) != 2 {
		t.Fatalf("expected global and member, got %d", len(read))
	}
	if read[0].Namespace() != "acme" || read[1].Namespace() != "acme-member-priya" {
		t.Errorf("read order wrong: %s, %s", read[0].Namespace(), read[1].Namespace())
	}
}

// Global has to be reachable deliberately, or a company fact learned in
// conversation can never be filed where it belongs.
func TestGlobalIsStillReachableWhileActingForSomeone(t *testing.T) {
	s, _ := scopes(t)
	if got := s.Global().Namespace(); got != "acme" {
		t.Errorf("global resolved to %q", got)
	}
}

// A team already keeping somebody's memory somewhere keeps it there.
func TestAnAdminsExplicitNamespaceWins(t *testing.T) {
	s, db := scopes(t)
	if err := db.SaveOrgMember(store.OrgMember{
		Org: "default", Member: "priya", Name: "Priya", Namespace: "priya-legacy-ns",
	}); err != nil {
		t.Fatal(err)
	}

	if got := s.MemberNamespace("priya"); got != "priya-legacy-ns" {
		t.Errorf("the configured namespace was ignored: %q", got)
	}
	// Somebody with no row still gets a predictable derived one.
	if got := s.MemberNamespace("kartik"); got != "acme-member-kartik" {
		t.Errorf("derived namespace is %q", got)
	}
}

// Namespaces become GitLoom repositories and local directory names, so a
// member id with a slash would otherwise write outside where it belongs.
func TestMemberIDsCannotEscapeTheirNamespace(t *testing.T) {
	s, _ := scopes(t)

	for id, want := range map[string]string{
		"priya":            "acme-member-priya",
		"Priya.S":          "acme-member-priya-s",
		"../../etc/passwd": "acme-member-etc-passwd",
		"a b":              "acme-member-a-b",
		"kartik@acme.com":  "acme-member-kartik-acme-com",
		"!!!":              "acme-member-unknown",
	} {
		got := s.MemberNamespace(id)
		if got != want {
			t.Errorf("%q -> %q, want %q", id, got, want)
		}
		if strings.Contains(got, "/") || strings.Contains(got, "..") {
			t.Errorf("%q produced a traversable namespace: %q", id, got)
		}
	}

	// No member is not a namespace.
	if got := s.MemberNamespace("  "); got != "" {
		t.Errorf("a blank member produced %q", got)
	}
}

// The global namespace falling back to the agent id is the existing behaviour
// and must not change for an install that never configured one.
func TestGlobalDefaultsToTheAgentWhenUnset(t *testing.T) {
	dir := t.TempDir()
	db, err := store.New(filepath.Join(dir, "s.db"), zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	f := NewFactory(dir, db, zap.NewNop())
	defer f.StopAll()

	s := NewScopes(f, db, "ocrew", "", zap.NewNop())
	if s.GlobalNamespace() != "ocrew" {
		t.Errorf("global is %q, want the agent id", s.GlobalNamespace())
	}
}
