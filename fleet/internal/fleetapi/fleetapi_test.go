package fleetapi_test

import (
	"context"
	"encoding/json"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/MelloB1989/karmax/fleet/internal/daemon"
	"github.com/MelloB1989/karmax/fleet/internal/fleetapi"
	"github.com/MelloB1989/karmax/fleet/internal/ledger"
)

var _ fleetapi.Client = (*fleetapi.Fake)(nil)

// jsonNames is the JSON keys a struct type encodes.
func jsonNames(t reflect.Type) []string {
	var out []string
	for i := 0; i < t.NumField(); i++ {
		tag := t.Field(i).Tag.Get("json")
		name, _, _ := strings.Cut(tag, ",")
		if name != "" && name != "-" {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// subset fails unless every key of from is one of to's: fleetd copies the
// daemon's and ledger's types into these, so the names must not drift.
func subset(t *testing.T, what string, from, to reflect.Type) {
	t.Helper()
	have := map[string]bool{}
	for _, n := range jsonNames(to) {
		have[n] = true
	}
	for _, n := range jsonNames(from) {
		if !have[n] {
			t.Errorf("%s: %s.%s has no field in %s", what, from.Name(), n, to.Name())
		}
	}
}

func TestTheContractKeepsTheDaemonsFieldNames(t *testing.T) {
	subset(t, "worker", reflect.TypeOf(daemon.Row{}), reflect.TypeOf(fleetapi.Worker{}))
	subset(t, "task", reflect.TypeOf(ledger.Task{}), reflect.TypeOf(fleetapi.Task{}))
	subset(t, "message", reflect.TypeOf(ledger.Relay{}), reflect.TypeOf(fleetapi.Message{}))
	subset(t, "task detail", reflect.TypeOf(daemon.TaskResult{}), reflect.TypeOf(fleetapi.TaskDetail{}))
}

// A daemon.Row encodes into a Worker without losing a field.
func TestARowDecodesAsAWorker(t *testing.T) {
	at := time.Date(2026, 10, 25, 9, 0, 0, 0, time.UTC)
	row := daemon.Row{Agent: "agent-03:2", SlotOf: "agent-03", Slot: 2, Host: "pc2", State: "working", Task: "t1",
		FiveHour: 42, FiveResets: at, Models: []string{"opus"}, Alerts: []string{}, Unreachable: true}
	b, err := json.Marshal(row)
	if err != nil {
		t.Fatal(err)
	}
	var w fleetapi.Worker
	if err := json.Unmarshal(b, &w); err != nil {
		t.Fatal(err)
	}
	if w.Agent != "agent-03:2" || w.SlotOf != "agent-03" || w.Slot != 2 || w.FiveHour != 42 || !w.FiveResets.Equal(at) || !w.Unreachable {
		t.Fatalf("worker = %+v", w)
	}
}

func TestTheFakesWatchWaitsForAStep(t *testing.T) {
	now := time.Date(2026, 10, 25, 21, 4, 0, 0, time.UTC)
	f := fleetapi.NewFake(fleetapi.RoleAdmin, func() time.Time { return now })
	s, err := f.Watch(context.Background(), 0)
	if err != nil || s.Seq == 0 {
		t.Fatalf("first watch: %v %+v", err, s)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if _, err := f.Watch(ctx, s.Seq); err == nil {
		t.Fatal("a watch at the current seq returned without a step")
	}
	go f.Step()
	s2, err := f.Watch(context.Background(), s.Seq)
	if err != nil || s2.Seq <= s.Seq {
		t.Fatalf("after a step: %v seq %d", err, s2.Seq)
	}
}

// A guest's fake fleet is what fleetd will give a guest: its lent agent,
// its own tasks and their events, no knocks, no access.
func TestTheFakeShowsAGuestOnlyItsOwn(t *testing.T) {
	now := time.Date(2026, 10, 25, 21, 4, 0, 0, time.UTC)
	f := fleetapi.NewFake(fleetapi.RoleGuest, func() time.Time { return now })
	s, _ := f.Watch(context.Background(), 0)
	if s.Me.Role != fleetapi.RoleGuest || s.Knocks != 0 {
		t.Fatalf("me = %+v knocks %d", s.Me, s.Knocks)
	}
	for _, w := range s.Workers {
		if w.SlotOf != "agent-09" {
			t.Errorf("guest sees %s", w.Agent)
		}
	}
	for _, task := range s.Tasks {
		if task.Assigner != s.Me.Name {
			t.Errorf("guest sees task %s of %s", task.ID, task.Assigner)
		}
	}
	for _, e := range s.Events {
		if e.Agent != "agent-09" {
			t.Errorf("guest sees event %s of %q", e.Kind, e.Agent)
		}
	}
	if _, err := f.Access(context.Background()); err == nil {
		t.Error("a guest read the access list")
	}
}

func TestTheFakeClosesATaskOnce(t *testing.T) {
	now := time.Date(2026, 10, 25, 21, 4, 0, 0, time.UTC)
	f := fleetapi.NewFake(fleetapi.RoleAdmin, func() time.Time { return now })
	ctx := context.Background()
	rep, err := f.Run(ctx, fleetapi.RunRequest{Brief: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Done(ctx, rep.Task); err != nil {
		t.Fatal(err)
	}
	if err := f.Done(ctx, rep.Task); err == nil {
		t.Fatal("closed a closed task")
	}
	if got := strings.Join(f.Calls, "|"); got != `run agent-08 "hello"|done `+rep.Task {
		t.Fatalf("calls = %s", got)
	}
}
