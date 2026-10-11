package console

import (
	"testing"
	"time"
)

func TestEventMergeDedupesLiveAndRecorded(t *testing.T) {
	at := time.Unix(1000, 0)
	live := []Event{{At: at, Agent: "agent-01", Session: "s", Kind: "Stop"}}
	rec := []Event{{ID: 42, At: at.Add(300 * time.Millisecond), Agent: "agent-01", Session: "s", Kind: "Stop"}}
	got := mergeEvents(live, rec)
	if len(got) != 1 || got[0].ID != 42 {
		t.Fatalf("merged = %+v, want one entry with fleetd's id", got)
	}
	// the other order of arrival is the same
	if got := mergeEvents(rec, live); len(got) != 1 || got[0].ID != 42 {
		t.Fatalf("merged = %+v", got)
	}
	// a different session or kind is a different event
	live = append(live, Event{At: at, Agent: "agent-01", Session: "other", Kind: "Stop"}, Event{At: at, Agent: "agent-01", Session: "s", Kind: "Notification"})
	if got := mergeEvents(live, rec); len(got) != 3 {
		t.Fatalf("merged = %+v", got)
	}
}

func TestEventMergeOrdersCapsAndFilters(t *testing.T) {
	var live, rec []Event
	for i := range 250 {
		rec = append(rec, Event{ID: int64(i + 1), At: time.Unix(int64(1000+i), 0), Agent: "a", Kind: "fleet.relay"})
	}
	live = append(live, Event{At: time.Unix(5000, 0), Agent: "a", Kind: "PreToolUse"}, Event{At: time.Unix(4000, 0), Agent: "a", Kind: "Stop"})
	got := mergeEvents(live, rec)
	if len(got) != StreamLen {
		t.Fatalf("len = %d", len(got))
	}
	if got[0].Kind != "Stop" || got[0].At.Unix() != 4000 {
		t.Fatalf("first = %+v: tool events must be dropped and the newest first", got[0])
	}
	for i := 1; i < len(got); i++ {
		if got[i].At.After(got[i-1].At) {
			t.Fatal("not newest first")
		}
	}
	if e := mergeEvents(nil, nil); e == nil || len(e) != 0 {
		t.Fatal("empty merge must be an empty non-nil slice")
	}
}
