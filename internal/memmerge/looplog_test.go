package memmerge

import (
	"testing"

	"github.com/MelloB1989/karmax/internal/store"
)

// A loop's log is a record of runs, not a set of facts: two runs that found
// the same thing are two runs, not a duplicate to consolidate. Merging them
// rewrote the record and cost a forget and a write per line.
func TestALoopsLogIsNotConsolidated(t *testing.T) {
	entries := []store.StoredMemoryEntry{
		{ID: "facts/context/night-watch/2026-10.md#2026-10-30-night-watch-quiet", Content: "(night-watch) quiet"},
		{ID: "facts/context/night-watch/2026-10.md#2026-10-31-night-watch-quiet", Content: "(night-watch) quiet"},
		{ID: "facts/projects/contoso.md#2026-10-30-renewal", Content: "Contoso renewal is due"},
		{ID: "facts/projects/contoso.md#2026-10-31-renewal", Content: "Contoso renewal is due soon"},
		{ID: "facts/projects/contoso.md#2026-10-29-pinned", Content: "Contoso is a client", Pinned: true},
	}
	got := mergeable(entries)
	total := 0
	for _, es := range got {
		total += len(es)
		for _, e := range es {
			if e.Pinned || e.ID[:22] == "facts/context/night-wa" {
				t.Errorf("%s was put up for merging", e.ID)
			}
		}
	}
	if total != 2 {
		t.Errorf("%d entries put up for merging, want the two Contoso facts", total)
	}
}
