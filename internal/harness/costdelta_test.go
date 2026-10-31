package harness

import (
	"context"
	"math"
	"os"
	"testing"
	"time"
)

// cumulativeCostClaude reports total_cost_usd the way the CLI does: the
// process's running total, not the turn's own cost.
func cumulativeCostClaude(t *testing.T) string {
	t.Helper()
	return writeScript(t, "n=0\nwhile IFS= read -r line; do n=$((n+1))\n"+
		"case $n in 1) c=0.01;; 2) c=0.03;; *) c=0.06;; esac\n"+
		`echo "{\"type\":\"result\",\"subtype\":\"success\",\"is_error\":false,\"result\":\"ok\",\"total_cost_usd\":$c,\"usage\":{}}"`+"\n"+
		"done\n")
}

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

// The health check's session was recorded at $120,111 for 5,471 one-word
// pings on the cheapest model, and the orchestrator's at $181 a turn: every
// turn added the process's running total again, so a session's recorded cost
// grew with the square of its turns. A turn costs what it added.
func TestATurnCostsWhatItAddedNotTheProcesssRunningTotal(t *testing.T) {
	st := newMemStore()
	sup := New(Config{
		Binary: cumulativeCostClaude(t), WorkdirRoot: t.TempDir(), MaxLive: 4, Env: os.Environ(),
		Policies: map[string]Policy{"k": {TurnTimeout: 5 * time.Second}},
	}, st, NewBreaker(0.95, nil), testLog{t}, nil)
	t.Cleanup(sup.Shutdown)

	for i, want := range []float64{0.01, 0.02, 0.03} {
		turn, err := sup.Send(context.Background(), "key", "k", "hi")
		if err != nil {
			t.Fatal(err)
		}
		if !near(turn.CostUSD, want) {
			t.Errorf("turn %d cost = %v, want %v", i+1, turn.CostUSD, want)
		}
	}
	if rec, _ := st.GetHarnessSession("key"); rec == nil || !near(rec.CostUSD, 0.06) {
		t.Errorf("session cost = %+v, want 0.06 (what the process reported in total)", rec)
	}
}

// A new process starts its own running total from zero, so the first turn
// after a resume is not charged as a negative or as the old process's total.
func TestTheRunningTotalStartsAgainWithEachProcess(t *testing.T) {
	st := newMemStore()
	sup := New(Config{
		Binary: cumulativeCostClaude(t), WorkdirRoot: t.TempDir(), MaxLive: 4, Env: os.Environ(),
		Policies: map[string]Policy{"k": {TurnTimeout: 5 * time.Second}},
	}, st, NewBreaker(0.95, nil), testLog{t}, nil)
	t.Cleanup(sup.Shutdown)

	for range 2 {
		if _, err := sup.Send(context.Background(), "key", "k", "hi"); err != nil {
			t.Fatal(err)
		}
	}
	sup.Close("key")
	turn, err := sup.Send(context.Background(), "key", "k", "hi")
	if err != nil {
		t.Fatal(err)
	}
	if !near(turn.CostUSD, 0.01) {
		t.Errorf("first turn of the resumed process cost %v, want 0.01", turn.CostUSD)
	}
}
