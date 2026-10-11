package console

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

// apply is RFC 6902 for the three ops Diff emits.
func apply(t *testing.T, doc []byte, ops []PatchOp) []byte {
	t.Helper()
	root, err := decode(doc)
	if err != nil {
		t.Fatal(err)
	}
	for _, op := range ops {
		if op.Path == "" {
			if op.Op != "replace" {
				t.Fatalf("root op %s", op.Op)
			}
			root, _ = decode(op.Value)
			continue
		}
		parts := strings.Split(op.Path[1:], "/")
		for i := range parts {
			parts[i] = strings.ReplaceAll(strings.ReplaceAll(parts[i], "~1", "/"), "~0", "~")
		}
		parent := root
		for _, p := range parts[:len(parts)-1] {
			switch c := parent.(type) {
			case map[string]any:
				parent = c[p]
			case []any:
				i, _ := strconv.Atoi(p)
				parent = c[i]
			}
		}
		last := parts[len(parts)-1]
		var val any
		if op.Op != "remove" {
			val, _ = decode(op.Value)
		}
		switch c := parent.(type) {
		case map[string]any:
			if op.Op == "remove" {
				delete(c, last)
			} else {
				if _, ok := c[last]; (op.Op == "replace") != ok {
					t.Fatalf("%s at %s: key present=%v", op.Op, op.Path, ok)
				}
				c[last] = val
			}
		case []any:
			i, _ := strconv.Atoi(last)
			if op.Op != "replace" {
				t.Fatalf("array op %s at %s", op.Op, op.Path)
			}
			c[i] = val
		}
	}
	b, _ := json.Marshal(root)
	return b
}

func fixtureSnapshot() Snapshot {
	at := time.Date(2026, 10, 11, 14, 0, 0, 0, time.UTC)
	s := Snapshot{At: at, Sources: map[string]Source{"host": {OK: true, At: at}, "fleetd": {OK: true, At: at}},
		Tasks:  []Task{{ID: "T1", Agent: "agent-01", Assigner: "operator", Since: at}, {ID: "T2", Agent: "agent-02", Since: at}},
		Events: []Event{{ID: 9007199254740993, At: at, Agent: "agent-01", Kind: "Stop"}}, Activity: map[string][]Bucket{"agent-01": {{T: at, N: 1}}}}
	for i := 1; i <= 4; i++ {
		s.Agents = append(s.Agents, Agent{ID: fmt.Sprintf("agent-0%d", i), Live: true, Phase: "standby", Alerts: []string{}, Models: []string{},
			Container: &Container{Running: true, At: at}})
	}
	return s
}

func TestDiffThenApplyEqualsNext(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	prev := fixtureSnapshot()
	for i := range 200 {
		next := prev
		next.Agents = append([]Agent(nil), prev.Agents...)
		next.Tasks = append([]Task(nil), prev.Tasks...)
		next.Events = append([]Event(nil), prev.Events...)
		next.At = prev.At.Add(time.Second)
		switch rng.Intn(7) {
		case 0:
			a := &next.Agents[rng.Intn(4)]
			a.Phase = []string{"working", "waiting", "standby", "cooling"}[rng.Intn(4)]
			a.ContextPct = float64(rng.Intn(100))
		case 1:
			next.Events = append([]Event{{ID: int64(1<<53 + rng.Intn(1000)), At: next.At, Agent: "agent-02", Kind: "fleet/odd~key"}}, next.Events...)
		case 2:
			if len(next.Tasks) > 0 {
				next.Tasks = next.Tasks[1:]
			}
		case 3:
			a := &next.Agents[rng.Intn(4)]
			if a.CurrentTool == nil {
				a.CurrentTool = &Tool{Name: "Bash", Since: next.At}
			} else {
				a.CurrentTool = nil
			}
		case 4:
			next.Sources = map[string]Source{"host": {OK: rng.Intn(2) == 0, Error: "x"}}
		case 5:
			a := &next.Agents[rng.Intn(4)]
			a.Container = &Container{Running: rng.Intn(2) == 0, Mem: int64(rng.Intn(1 << 30)), At: next.At}
		case 6:
			next.Tasks = append(next.Tasks, Task{ID: fmt.Sprint("T", i), Since: next.At})
		}
		pb, _ := json.Marshal(prev)
		nb, _ := json.Marshal(next)
		ops, err := Diff(pb, nb)
		if err != nil {
			t.Fatal(err)
		}
		got := apply(t, pb, ops)
		var g, w any
		json.Unmarshal(got, &g)
		json.Unmarshal(nb, &w)
		if !reflect.DeepEqual(g, w) {
			t.Fatalf("step %d: apply(prev, diff) != next\nops=%v\ngot =%s\nwant=%s", i, ops, got, nb)
		}
		prev = next
	}
}

func TestDiffOfEqualDocumentsIsEmpty(t *testing.T) {
	b, _ := json.Marshal(fixtureSnapshot())
	ops, err := Diff(b, b)
	if err != nil || ops == nil || len(ops) != 0 {
		t.Fatalf("ops = %v, %v", ops, err)
	}
}
