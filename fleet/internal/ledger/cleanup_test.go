package ledger

import "testing"

// A kept item is told once, whatever the reason later passes keep it for.
func TestAKeptItemIsToldOnce(t *testing.T) {
	db := open(t)
	first, err := db.KeptTold("agent-03", "/work/wt/x", t0)
	if err != nil || !first {
		t.Fatalf("first: %v %v", first, err)
	}
	if first, err := db.KeptTold("agent-03", "/work/wt/x", t0.Add(24*60*60*1e9)); err != nil || first {
		t.Fatalf("again: %v %v", first, err)
	}
	if first, err := db.KeptTold("agent-04", "/work/wt/x", t0); err != nil || !first {
		t.Fatalf("another agent's: %v %v", first, err)
	}
	// Gone and back is a new item.
	if err := db.ForgetKept("agent-03", []string{"/work/wt/y"}); err != nil {
		t.Fatal(err)
	}
	if first, err := db.KeptTold("agent-03", "/work/wt/x", t0); err != nil || !first {
		t.Fatalf("after it was forgotten: %v %v", first, err)
	}
}
