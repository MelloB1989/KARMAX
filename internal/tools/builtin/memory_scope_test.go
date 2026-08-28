package builtin

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MelloB1989/karmax/internal/memory"
	"github.com/MelloB1989/karmax/internal/store"
	"github.com/MelloB1989/karmax/internal/tools"
	"github.com/MelloB1989/karmax/pkg/connectorkit"
	"go.uber.org/zap"
)

func ingestTool(t *testing.T) (*MemoryIngestTool, *memory.Scopes, *store.Store) {
	t.Helper()
	dir := t.TempDir()
	db, err := store.New(filepath.Join(dir, "m.db"), zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	f := memory.NewFactory(dir, db, zap.NewNop())
	t.Cleanup(f.StopAll)

	sc := memory.NewScopes(f, db, "ocrew", "acme", zap.NewNop())
	return &MemoryIngestTool{
		Store: db, MemoryMgr: sc.Global(), AgentID: "ocrew", Scopes: sc,
	}, sc, db
}

func ingest(t *testing.T, tool *MemoryIngestTool, ctx context.Context, content string, extra map[string]any) tools.ToolResult {
	t.Helper()
	in := map[string]any{"content": content, "category": "context"}
	for k, v := range extra {
		in[k] = v
	}
	res, err := tool.Execute(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

// The claim this whole design rests on: something learned while helping a
// person does not become readable by every agent, recipe and colleague.
func TestAPrivateRemarkDoesNotReachTheSharedNamespace(t *testing.T) {
	tool, sc, _ := ingestTool(t)
	ctx := connectorkit.WithActor(context.Background(), "priya")

	ingest(t, tool, ctx, "Priya is interviewing elsewhere and asked me not to mention it", nil)

	// In her namespace.
	hers, err := sc.ForMember("priya").SearchSemantic("interviewing", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(hers) == 0 {
		t.Fatal("the fact was not stored in the member's namespace at all")
	}

	// And nowhere else.
	shared, err := sc.Global().SearchSemantic("interviewing", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(shared) != 0 {
		t.Errorf("a private remark is readable in the shared namespace: %d hits", len(shared))
	}
}

// Filing a company fact has to be possible, or org knowledge can never be
// written down during a conversation.
func TestOrgScopeReachesTheSharedNamespace(t *testing.T) {
	tool, sc, _ := ingestTool(t)
	ctx := connectorkit.WithActor(context.Background(), "priya")

	ingest(t, tool, ctx, "Deploys go out on Tuesdays", map[string]any{"scope": "org"})

	shared, _ := sc.Global().SearchSemantic("Deploys", 10)
	if len(shared) == 0 {
		t.Error("an explicitly org-scoped fact did not reach the shared namespace")
	}
	hers, _ := sc.ForMember("priya").SearchSemantic("Deploys", 10)
	if len(hers) != 0 {
		t.Error("an org fact was also written to the member's namespace")
	}
}

// Anything other than an explicit "org" keeps it with the person. A model that
// invents a scope value must not thereby publish something.
func TestOnlyTheExactOrgScopePublishes(t *testing.T) {
	for _, scope := range []string{"", "person", "global", "shared", "company", "ORG "} {
		tool, sc, _ := ingestTool(t)
		ctx := connectorkit.WithActor(context.Background(), "priya")
		ingest(t, tool, ctx, "something confidential", map[string]any{"scope": scope})

		shared, _ := sc.Global().SearchSemantic("confidential", 10)
		// "ORG " with whitespace and different case IS the deliberate act, so
		// it is allowed; everything else must stay private.
		wantShared := strings.EqualFold(strings.TrimSpace(scope), "org")
		if (len(shared) > 0) != wantShared {
			t.Errorf("scope %q: shared=%v, want %v", scope, len(shared) > 0, wantShared)
		}
	}
}

// Work on nobody's behalf — a cron tick, a webhook, a recipe — writes to the
// organisation, because there is no person for it to belong to.
func TestWorkWithNoActorWritesToTheOrganisation(t *testing.T) {
	tool, sc, _ := ingestTool(t)

	ingest(t, tool, context.Background(), "The nightly build started failing", nil)

	shared, _ := sc.Global().SearchSemantic("nightly build", 10)
	if len(shared) == 0 {
		t.Error("a fact learned with no acting member did not reach the organisation")
	}
}

// Dedup has to run against the namespace being written to, or a fact already
// known about a person is compared with the company's memory and saved again.
func TestDeduplicationHappensWithinTheRightNamespace(t *testing.T) {
	tool, sc, _ := ingestTool(t)
	ctx := connectorkit.WithActor(context.Background(), "priya")

	ingest(t, tool, ctx, "Priya prefers morning standups", nil)
	res := ingest(t, tool, ctx, "Priya prefers morning standups", nil)

	out := strings.ToLower(fmt.Sprint(res.Output))
	if !strings.Contains(out, "similar") && !strings.Contains(out, "already") {
		t.Errorf("the duplicate was not detected: %q", out)
	}
	hers, _ := sc.ForMember("priya").SearchSemantic("standups", 10)
	if len(hers) > 1 {
		t.Errorf("the same fact was stored %d times", len(hers))
	}
}
