package harness

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// newResidentSupervisor has one resident kind ("orch") and one ordinary kind
// ("chat"), both with an idle window short enough to be past at once.
func newResidentSupervisor(t *testing.T, maxLive int) (*Supervisor, *memStore) {
	t.Helper()
	st := newMemStore()
	sup := New(Config{
		Binary: writeFakeClaude(t, time.Millisecond), WorkdirRoot: t.TempDir(), MaxLive: maxLive,
		Env: os.Environ(),
		Policies: map[string]Policy{
			"orch": {Idle: time.Millisecond, TurnTimeout: 5 * time.Second, Resident: true},
			"chat": {Idle: time.Millisecond, TurnTimeout: 5 * time.Second},
		},
	}, st, NewBreaker(0.95, nil), testLog{t}, nil)
	t.Cleanup(sup.Shutdown)
	return sup, st
}

func mustSend(t *testing.T, sup *Supervisor, key, kind string) {
	t.Helper()
	if _, err := sup.Send(context.Background(), key, kind, "hi"); err != nil {
		t.Fatalf("send %s: %v", key, err)
	}
}

// A resident session is never closed for being idle: closing it would take
// away its inbox, and every session messaging it would bounce.
func TestReapLeavesAResidentSessionAlone(t *testing.T) {
	sup, _ := newResidentSupervisor(t, 8)
	mustSend(t, sup, "orch", "orch")
	mustSend(t, sup, "c", "chat")

	sup.Reap(time.Now().Add(time.Hour))

	if live := sup.Live(); !slices.Equal(live, []string{"orch"}) {
		t.Fatalf("live after reap = %q, want only the resident session", live)
	}
}

// A resident session neither counts toward max_live nor is ever chosen as the
// least recently used one to evict.
func TestEvictionSkipsAndDoesNotCountResidentSessions(t *testing.T) {
	sup, _ := newResidentSupervisor(t, 1)
	mustSend(t, sup, "orch", "orch")
	mustSend(t, sup, "c1", "chat") // the resident does not fill the one slot
	if live := sup.Live(); !slices.Equal(live, []string{"c1", "orch"}) {
		t.Fatalf("live = %q, want both: the resident must not count toward max_live", live)
	}
	mustSend(t, sup, "c2", "chat") // now full: c1 goes, the resident stays
	if live := sup.Live(); !slices.Equal(live, []string{"c2", "orch"}) {
		t.Fatalf("live = %q, want c2 and orch", live)
	}
}

// A resident session that died — a failed turn, a crash, a daemon restart —
// comes back without waiting for someone to message it, so its inbox is bound
// again. It comes back idle, not claimed by a turn nobody is running.
func TestReviveBringsBackADeadResidentSession(t *testing.T) {
	sup, st := newResidentSupervisor(t, 8)
	mustSend(t, sup, "orch", "orch")
	sup.kill("orch", HarnessDead, "crashed")

	sup.ReviveResident(context.Background())

	if live := sup.Live(); !slices.Equal(live, []string{"orch"}) {
		t.Fatalf("live = %q, want the resident session back", live)
	}
	if sup.Busy("orch") {
		t.Error("a revived session must be idle, or nothing could ever reap or reuse it")
	}
	if rec, _ := st.GetHarnessSession("orch"); rec.State != HarnessLive {
		t.Errorf("state = %q, want live", rec.State)
	}
	// And it is the same conversation, resumed rather than started over.
	mustSend(t, sup, "orch", "orch")
}

// An operator who closed a resident session meant it (the harness.close tool).
func TestReviveLeavesARetiredResidentSessionClosed(t *testing.T) {
	sup, _ := newResidentSupervisor(t, 8)
	mustSend(t, sup, "orch", "orch")
	sup.Retire("orch")

	sup.ReviveResident(context.Background())

	if live := sup.Live(); len(live) != 0 {
		t.Fatalf("live = %q, want nothing: a closed session stays closed", live)
	}
}

// Only resident kinds are revived; everything else waits for its next message
// exactly as before.
func TestReviveIgnoresOrdinaryKinds(t *testing.T) {
	sup, _ := newResidentSupervisor(t, 8)
	mustSend(t, sup, "c", "chat")
	sup.kill("c", HarnessDead, "crashed")

	sup.ReviveResident(context.Background())

	if live := sup.Live(); len(live) != 0 {
		t.Fatalf("live = %q, want nothing", live)
	}
}

// A daemon stopping is not an operator closing the orchestrator: its row is
// left for the next start to revive.
func TestShutdownLeavesResidentSessionsRevivable(t *testing.T) {
	sup, st := newResidentSupervisor(t, 8)
	mustSend(t, sup, "orch", "orch")
	mustSend(t, sup, "c", "chat")

	sup.Shutdown()

	if rec, _ := st.GetHarnessSession("orch"); rec.State != HarnessDead {
		t.Errorf("resident state after shutdown = %q, want dead (revivable)", rec.State)
	}
	if rec, _ := st.GetHarnessSession("c"); rec.State != HarnessClosed {
		t.Errorf("chat state after shutdown = %q, want closed as before", rec.State)
	}
}

// Everything else that closes a session does it on the way to the next one —
// a model change, the turn limit, a browser recycle — and a resident session
// must come back from it rather than lose its inbox until someone writes.
func TestAutomaticClosesLeaveAResidentSessionRevivable(t *testing.T) {
	for name, closeIt := range map[string]func(*Supervisor){
		"Close":       func(s *Supervisor) { s.Close("orch") },
		"CloseIfIdle": func(s *Supervisor) { s.CloseIfIdle("orch") },
	} {
		sup, st := newResidentSupervisor(t, 8)
		mustSend(t, sup, "orch", "orch")
		closeIt(sup)
		if rec, _ := st.GetHarnessSession("orch"); rec.State != HarnessDead {
			t.Errorf("%s: state %q, want dead (revivable)", name, rec.State)
		}
		sup.ReviveResident(context.Background())
		if live := sup.Live(); !slices.Equal(live, []string{"orch"}) {
			t.Errorf("%s: not revived: %q", name, live)
		}
	}
}

func TestAResidentSessionAtItsTurnLimitComesBackFresh(t *testing.T) {
	st := newMemStore()
	sup := New(Config{
		Binary: writeFakeClaude(t, time.Millisecond), WorkdirRoot: t.TempDir(), MaxLive: 4, Env: os.Environ(),
		Policies: map[string]Policy{"orch": {TurnTimeout: 5 * time.Second, Resident: true, MaxTurns: 1}},
	}, st, NewBreaker(0.95, nil), testLog{t}, nil)
	t.Cleanup(sup.Shutdown)
	mustSend(t, sup, "orch", "orch")
	first, _ := st.GetHarnessSession("orch")
	sup.ReviveResident(context.Background())
	if live := sup.Live(); !slices.Equal(live, []string{"orch"}) {
		t.Fatalf("not revived after its turn limit: %q", live)
	}
	if now, _ := st.GetHarnessSession("orch"); now.HarnessSessionID == first.HarnessSessionID {
		t.Error("revived on the same transcript past its turn limit; it should start fresh")
	}
}

// ReviveResident and a message arriving at the same moment — the usual case
// at daemon start — must open one process, not two on one transcript with one
// of them never closed.
func TestReviveAndASendAtOnceOpenOneProcess(t *testing.T) {
	spawns := filepath.Join(t.TempDir(), "spawns")
	bin := writeScript(t, "echo $$ >> "+spawns+"\nwhile IFS= read -r line; do "+resultLine+"done\n")
	st := newMemStore()
	sup := New(Config{
		Binary: bin, WorkdirRoot: t.TempDir(), MaxLive: 8, Env: os.Environ(),
		Policies: map[string]Policy{"orch": {TurnTimeout: 5 * time.Second, Resident: true}},
	}, st, NewBreaker(0.95, nil), testLog{t}, nil)
	t.Cleanup(sup.Shutdown)
	mustSend(t, sup, "orch", "orch")
	for i := 0; i < 15; i++ {
		sup.kill("orch", HarnessDead, "crashed")
		_ = os.Remove(spawns)
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); sup.ReviveResident(context.Background()) }()
		go func() { defer wg.Done(); mustSend(t, sup, "orch", "orch") }()
		wg.Wait()
		time.Sleep(50 * time.Millisecond)
		b, _ := os.ReadFile(spawns)
		if n := strings.Count(string(b), "\n"); n != 1 {
			t.Fatalf("round %d: %d processes opened for one session", i, n)
		}
	}
}
