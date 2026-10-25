package ledger

import (
	"slices"
	"testing"
	"time"

	"github.com/MelloB1989/karmax/fleet/internal/fleetapi"
	"github.com/MelloB1989/karmax/fleet/internal/tailnet"
)

func TestADeviceTagReplacesItsRoleAndAgents(t *testing.T) {
	db := open(t)
	d := fleetapi.Device{StableID: "nF1", Name: "friend-laptop.tail1234.ts.net", Login: "friend@example.com",
		Role: fleetapi.RoleGuest, Agents: []string{"agent-10", "agent-09"}, TaggedAt: t0, TaggedBy: "operator"}
	if err := db.Tag(d); err != nil {
		t.Fatal(err)
	}
	got, ok, err := db.Device("nF1")
	if err != nil || !ok || got.Role != fleetapi.RoleGuest || !slices.Equal(got.Agents, []string{"agent-09", "agent-10"}) || !got.TaggedAt.Equal(t0) {
		t.Fatalf("device: %+v %v %v", got, ok, err)
	}
	d.Role, d.Agents = fleetapi.RoleAdmin, nil
	if err := db.Tag(d); err != nil {
		t.Fatal(err)
	}
	if got, _, _ := db.Device("nF1"); got.Role != fleetapi.RoleAdmin || len(got.Agents) != 0 {
		t.Fatalf("retagged: %+v", got)
	}
	if err := db.Untag("nF1"); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := db.Device("nF1"); ok {
		t.Fatal("untagged device still there")
	}
	var n int
	if err := db.sql.QueryRow(`SELECT count(*) FROM device_agents`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("lends left behind: %d %v", n, err)
	}
}

func TestADeviceIsSeenAtMostOnceAMinute(t *testing.T) {
	db := open(t)
	if err := db.Tag(fleetapi.Device{StableID: "nF1", Name: "f", Login: "friend@example.com", Role: fleetapi.RoleGuest, TaggedAt: t0, TaggedBy: "operator"}); err != nil {
		t.Fatal(err)
	}
	for _, at := range []time.Time{t0, t0.Add(30 * time.Second)} {
		if err := db.Seen("nF1", at); err != nil {
			t.Fatal(err)
		}
	}
	if d, _, _ := db.Device("nF1"); !d.LastSeen.Equal(t0) {
		t.Fatalf("seen: %v", d.LastSeen)
	}
	_ = db.Seen("nF1", t0.Add(time.Minute))
	if d, _, _ := db.Device("nF1"); !d.LastSeen.Equal(t0.Add(time.Minute)) {
		t.Fatalf("seen a minute on: %v", d.LastSeen)
	}
}

func TestKnocksCountAndListNewestFirst(t *testing.T) {
	db := open(t)
	a := tailnet.Who{StableID: "nA", Login: "a@example.com", Node: "a.tail1234.ts.net"}
	b := tailnet.Who{StableID: "nB", Login: "b@example.com", Node: "b.tail1234.ts.net"}
	for i, w := range []tailnet.Who{a, b, a} {
		if _, err := db.Knock(w, t0.Add(time.Duration(i)*time.Minute)); err != nil {
			t.Fatal(err)
		}
	}
	ks, err := db.Knocks(0)
	if err != nil || len(ks) != 2 || ks[0].StableID != "nA" || ks[0].Count != 2 || !ks[0].First.Equal(t0) || ks[1].Count != 1 {
		t.Fatalf("knocks: %+v %v", ks, err)
	}
	if ks, _ := db.Knocks(1); len(ks) != 1 {
		t.Fatalf("limit: %+v", ks)
	}
	// A tag ends the knocking.
	if err := db.Tag(fleetapi.Device{StableID: "nA", Name: a.Node, Login: a.Login, Role: fleetapi.RoleAdmin, TaggedAt: t0, TaggedBy: "operator"}); err != nil {
		t.Fatal(err)
	}
	if ks, _ := db.Knocks(0); len(ks) != 1 || ks[0].StableID != "nB" {
		t.Fatalf("after a tag: %+v", ks)
	}
}

func TestATaskReopenedIsNotAdminWorkUntilSaid(t *testing.T) {
	db := open(t)
	if err := db.OpenTask("T1", "agent-03", "operator", t0); err != nil {
		t.Fatal(err)
	}
	if tk, _ := db.Task("T1"); tk.AdminWork {
		t.Fatal("a new task is admin work by default")
	}
	_ = db.SetTaskAdminWork("T1", true)
	if tk, _ := db.Task("T1"); !tk.AdminWork {
		t.Fatal("admin work not kept")
	}
	if ts, _ := db.Tasks(true, "agent-03", 0); len(ts) != 1 || !ts[0].AdminWork {
		t.Fatalf("tasks: %+v", ts)
	}
	_ = db.OpenTask("T1", "agent-03", "friend@example.com", t0.Add(time.Hour))
	if tk, _ := db.Task("T1"); tk.AdminWork {
		t.Fatal("a reopened task kept the last assignment's admin work")
	}
}
