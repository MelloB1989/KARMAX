package memmerge

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MelloB1989/karmax/internal/memory"
	"github.com/MelloB1989/karmax/internal/store"
	"go.uber.org/zap"
)

// slowOps changes the local store as slowly as the live one answers: every
// forget waits for the store to apply it, and a write is a whole round trip.
type slowOps struct {
	s     *store.Store
	delay time.Duration

	mu     sync.Mutex
	forgot []string
	wrote  []string
}

func (o *slowOps) Forget(id string) error {
	time.Sleep(o.delay)
	o.mu.Lock()
	o.forgot = append(o.forgot, id)
	o.mu.Unlock()
	return o.s.DeleteMemoryEntry(id)
}

func (o *slowOps) Write(e memory.MemoryEntry) error {
	time.Sleep(o.delay)
	o.mu.Lock()
	o.wrote = append(o.wrote, e.Content)
	o.mu.Unlock()
	return nil
}

func (o *slowOps) state() (forgot map[string]int, wrote []string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	forgot = map[string]int{}
	for _, id := range o.forgot {
		forgot[id]++
	}
	return forgot, append([]string(nil), o.wrote...)
}

var numbered = regexp.MustCompile(`(?m)^(\d+)\. (.+)$`)

// pairingModel answers a merge prompt by pairing the entries it is shown, in
// order — "1 with 2", "3 with 4" — and remembers which ids each pair named.
type pairingModel struct {
	asked atomic.Int32
	ids   map[string]string // content -> id

	mu    sync.Mutex
	pairs map[string][]string // merged fact -> the ids it replaces
}

func (p *pairingModel) ask(_ context.Context, _, prompt string) (string, bool) {
	p.asked.Add(1)
	type merge struct {
		Fact     string `json:"fact"`
		Replaces []int  `json:"replaces"`
	}
	var out struct {
		Merges []merge `json:"merges"`
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.pairs = map[string][]string{}
	shown := numbered.FindAllStringSubmatch(prompt, -1)
	for i := 0; i+1 < len(shown); i += 2 {
		var a, b int
		fmt.Sscan(shown[i][1], &a)
		fmt.Sscan(shown[i+1][1], &b)
		fact := fmt.Sprintf("merged pair %d", len(out.Merges)+1)
		out.Merges = append(out.Merges, merge{Fact: fact, Replaces: []int{a, b}})
		p.pairs[fact] = []string{p.ids[strings.TrimSpace(shown[i][2])], p.ids[strings.TrimSpace(shown[i+1][2])]}
	}
	body, _ := json.Marshal(out)
	return string(body), true
}

// contosoMemory is a store holding n near-duplicate facts about one subject.
func contosoMemory(t *testing.T, n int) (*store.Store, map[string]string) {
	t.Helper()
	s, err := store.New(filepath.Join(t.TempDir(), "k.db"), zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	ids := map[string]string{}
	for i := 1; i <= n; i++ {
		id := fmt.Sprintf("facts/projects/contoso.md#2026-10-%02d-renewal", i)
		content := fmt.Sprintf("Contoso renewal note %02d", i)
		ids[content] = id
		if err := s.InsertMemoryEntry(store.StoredMemoryEntry{ID: id, AgentID: "agent-1", Namespace: "ns",
			Role: "user", Content: content, Category: "project"}); err != nil {
			t.Fatal(err)
		}
	}
	return s, ids
}

// A pass that runs out of time stops between clusters, never inside one, and
// the next run finishes what it planned — without asking the model again.
//
// memory-merge ran nineteen minutes on a fourteen-minute deadline: every forget
// waits for the store to apply it, and a pass applied cluster after cluster
// with no look at the clock.
func TestAMergeStopsAtItsDeadlineAndTheNextRunFinishesIt(t *testing.T) {
	s, ids := contosoMemory(t, 8)
	model := &pairingModel{ids: ids}
	mg := New(Config{Namespace: "ns", Ask: model.ask}, s, nil, zap.NewNop())
	ops := &slowOps{s: s, delay: 40 * time.Millisecond}
	mg.ops = ops
	mg.opFloor = 40 * time.Millisecond

	// Four clusters of two take 480ms; this run has 300.
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	began := time.Now()
	if _, err := mg.Tick(ctx); err != nil {
		t.Fatalf("a pass that ran out of time failed: %v", err)
	}
	if took := time.Since(began); took > 300*time.Millisecond+30*time.Millisecond {
		t.Errorf("the pass took %v on a 300ms deadline", took)
	}
	forgot, wrote := ops.state()
	if len(wrote) == 0 || len(wrote) == 4 {
		t.Fatalf("%d of 4 clusters applied in the first run; want some, not all", len(wrote))
	}
	assertWhole(t, model, forgot, wrote)

	// The next run, with time to spare.
	if _, err := mg.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	forgot, wrote = ops.state()
	if len(wrote) != 4 {
		t.Errorf("%d of 4 clusters applied after the second run", len(wrote))
	}
	assertWhole(t, model, forgot, wrote)
	if n := model.asked.Load(); n != 1 {
		t.Errorf("the model was asked %d times; the second run should finish the first run's plan", n)
	}
	for id, n := range forgot {
		if n > 1 {
			t.Errorf("%s was forgotten %d times", id, n)
		}
	}
}

// A planned cluster whose entries changed before the next run got to it is
// dropped, not applied to what is there now.
func TestACarriedClusterWhoseEntriesChangedIsDropped(t *testing.T) {
	s, ids := contosoMemory(t, 8)
	model := &pairingModel{ids: ids}
	mg := New(Config{Namespace: "ns", Ask: model.ask}, s, nil, zap.NewNop())
	ops := &slowOps{s: s}
	mg.ops = ops
	// No cluster fits in what this run has left.
	mg.opFloor = time.Hour

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if _, err := mg.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if _, wrote := ops.state(); len(wrote) != 0 {
		t.Fatalf("a cluster was applied with no time for it: %v", wrote)
	}

	// Meanwhile one entry of the first pair is corrected elsewhere.
	model.mu.Lock()
	changed := model.pairs["merged pair 1"]
	model.mu.Unlock()
	if err := s.UpdateMemoryEntry(changed[0], "Contoso renewal moved to December"); err != nil {
		t.Fatal(err)
	}

	mg.opFloor = 0
	if _, err := mg.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	forgot, wrote := ops.state()
	for _, w := range wrote {
		if strings.Contains(w, "merged pair 1") {
			t.Errorf("a cluster whose entry changed was still applied: %s", w)
		}
	}
	if forgot[changed[1]] > 0 || forgot[changed[0]] > 0 {
		t.Errorf("an entry of the dropped cluster was forgotten: %v", forgot)
	}
	if len(wrote) != 3 {
		t.Errorf("%d clusters applied, want the three that did not change", len(wrote))
	}
}

// assertWhole checks that every cluster is applied whole or not at all.
func assertWhole(t *testing.T, model *pairingModel, forgot map[string]int, wrote []string) {
	t.Helper()
	model.mu.Lock()
	defer model.mu.Unlock()
	for fact, pair := range model.pairs {
		written := false
		for _, w := range wrote {
			written = written || strings.HasSuffix(w, " "+fact)
		}
		gone := 0
		for _, id := range pair {
			if forgot[id] > 0 {
				gone++
			}
		}
		if written && gone != len(pair) || !written && gone != 0 {
			t.Errorf("%q was left half-done: written=%v, %d of %d originals forgotten", fact, written, gone, len(pair))
		}
	}
}
