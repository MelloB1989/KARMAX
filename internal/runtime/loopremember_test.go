package runtime

import (
	"path/filepath"
	"testing"

	"github.com/MelloB1989/karmax/internal/memory"
	"github.com/MelloB1989/karmax/internal/store"
	"go.uber.org/zap"
)

// What a loop remembers about its runs is its log, filed under the loop's log
// policy — not a fact folded into one ever-growing document.
func TestALoopRemembersIntoItsLog(t *testing.T) {
	dir := t.TempDir()
	db, err := store.New(filepath.Join(dir, "k.db"), zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	mgr := memory.NewManager("agent-1", "ns", dir, db, zap.NewNop())
	t.Cleanup(mgr.Stop)

	k := &loopKit{loopName: "night-watch", agentID: "agent-1", mem: mgr}
	if err := k.Remember("(night-watch, 30 Oct 22:10) relay answered in 120 ms"); err != nil {
		t.Fatal(err)
	}
	res, err := mgr.SearchSemantic("relay answered", 5)
	if err != nil || len(res) == 0 {
		t.Fatalf("search = %v, %v", res, err)
	}
	if got := res[0].Entry; !memory.IsLoopLog(got) {
		t.Errorf("the loop's line was filed as %q, not as its log", got.Category)
	}
}
