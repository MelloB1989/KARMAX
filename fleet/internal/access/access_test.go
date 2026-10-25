package access_test

import (
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/MelloB1989/karmax/fleet/internal/access"
	"github.com/MelloB1989/karmax/fleet/internal/fleetapi"
	"github.com/MelloB1989/karmax/fleet/internal/ledger"
	"github.com/MelloB1989/karmax/fleet/internal/tailnet"
)

var t0 = time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)

var (
	friend = tailnet.Who{StableID: "nFRIEND1", Login: "friend@example.com", Node: "friend-laptop.tail1234.ts.net"}
	op     = tailnet.Who{StableID: "nOP1", Login: "op@example.com", Node: "op-mac.tail1234.ts.net"}
)

func store(t *testing.T) *ledger.DB {
	t.Helper()
	db, err := ledger.Open(filepath.Join(t.TempDir(), "fleet.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// sandbox: agent-09 and agent-10 are sandboxes; the rest are ordinary.
func sandbox(a string) bool { return a == "agent-09" || a == "agent-10" }

func change(t *testing.T, s access.Store, op, device string, agents ...string) (fleetapi.AccessList, error) {
	t.Helper()
	return access.Change(s, fleetapi.AccessChange{Op: op, Device: device, Agents: agents}, "operator", sandbox, t0)
}

func knock(t *testing.T, s access.Store, w tailnet.Who, at time.Time) {
	t.Helper()
	if err := access.Knocked(s, w, at, nil); err != nil {
		t.Fatal(err)
	}
}

func TestAnUntaggedDeviceIsRefusedAndKnocks(t *testing.T) {
	s := store(t)
	d, err := access.Resolve(s, friend)
	if err != nil || d.Role != "" {
		t.Fatalf("untagged: %+v %v", d, err)
	}
	if msg := access.Refusal(friend); !strings.Contains(msg, "this device (friend-laptop.tail1234.ts.net, friend@example.com) may not use this fleet yet.") ||
		!strings.Contains(msg, "fleetctl access lend friend-laptop <agent>") {
		t.Fatalf("refusal: %s", msg)
	}
	knock(t, s, friend, t0)
	knock(t, s, friend, t0.Add(time.Minute))
	l, err := access.List(s)
	if err != nil || len(l.Knocks) != 1 || l.Knocks[0].Count != 2 || l.Knocks[0].Name != friend.Node || l.Knocks[0].Login != friend.Login {
		t.Fatalf("knocks: %+v %v", l, err)
	}
	// Tagged, it is let in and leaves the knocks.
	if _, err := change(t, s, fleetapi.AccessLend, "friend-laptop", "agent-09"); err != nil {
		t.Fatal(err)
	}
	d, err = access.Resolve(s, friend)
	if err != nil || d.Role != fleetapi.RoleGuest || !slices.Equal(d.Agents, []string{"agent-09"}) {
		t.Fatalf("lent: %+v %v", d, err)
	}
	if l, _ := access.List(s); len(l.Knocks) != 0 || len(l.Devices) != 1 {
		t.Fatalf("after the tag: %+v", l)
	}
}

func TestATagFollowsTheStableIDNotTheName(t *testing.T) {
	s := store(t)
	knock(t, s, op, t0)
	if _, err := change(t, s, fleetapi.AccessAdmin, "op-mac"); err != nil {
		t.Fatal(err)
	}
	if d, _ := access.Resolve(s, op); d.Role != fleetapi.RoleAdmin {
		t.Fatalf("the admin device: %+v", d)
	}
	for name, w := range map[string]tailnet.Who{
		"the same name, another StableID":   {StableID: "nEVIL", Login: "op@example.com", Node: op.Node},
		"the same name, another login":      {StableID: "nEVIL", Login: "eve@example.com", Node: op.Node},
		"the StableID, another owner":       {StableID: op.StableID, Login: "eve@example.com", Node: op.Node},
		"the StableID, no login (a tagged)": {StableID: op.StableID, Node: op.Node},
		"no StableID":                       {Login: op.Login, Node: op.Node},
	} {
		if d, err := access.Resolve(s, w); err != nil || d.Role != "" {
			t.Errorf("%s: %+v %v", name, d, err)
		}
	}
	// A rename of the tagged device itself keeps its tag, and shows the new name.
	renamed := op
	renamed.Node = "op-mac-2.tail1234.ts.net"
	if d, _ := access.Resolve(s, renamed); d.Role != fleetapi.RoleAdmin || d.Name != renamed.Node {
		t.Fatalf("renamed: %+v", d)
	}
}

func TestOnlySandboxAgentsCanBeLent(t *testing.T) {
	s := store(t)
	knock(t, s, friend, t0)
	if _, err := change(t, s, fleetapi.AccessLend, "friend-laptop", "agent-09", "agent-03"); !errors.Is(err, access.ErrBadChange) || !strings.Contains(err.Error(), "agent-03 is not a sandbox") {
		t.Fatalf("lend an ordinary agent: %v", err)
	}
	if d, ok, _ := s.Device(friend.StableID); ok {
		t.Fatalf("a refused lend tagged the device: %+v", d)
	}
	// A slot is its agent's: lent as the agent.
	l, err := change(t, s, fleetapi.AccessLend, "friend-laptop", "agent-10:2", "agent-09")
	if err != nil || !slices.Equal(l.Devices[0].Agents, []string{"agent-09", "agent-10"}) {
		t.Fatalf("lend: %+v %v", l, err)
	}
	l, err = change(t, s, fleetapi.AccessUnlend, "friend-laptop", "agent-10")
	if err != nil || !slices.Equal(l.Devices[0].Agents, []string{"agent-09"}) {
		t.Fatalf("unlend: %+v %v", l, err)
	}
}

func TestAdminOnAGuestDropsItsLends(t *testing.T) {
	s := store(t)
	knock(t, s, friend, t0)
	if _, err := change(t, s, fleetapi.AccessLend, "friend-laptop", "agent-09"); err != nil {
		t.Fatal(err)
	}
	if _, err := change(t, s, fleetapi.AccessAdmin, "friend-laptop"); err != nil {
		t.Fatal(err)
	}
	d, _ := access.Resolve(s, friend)
	if d.Role != fleetapi.RoleAdmin || len(d.Agents) != 0 {
		t.Fatalf("admin: %+v", d)
	}
	if _, err := change(t, s, fleetapi.AccessRevoke, "friend-laptop"); err != nil {
		t.Fatal(err)
	}
	if d, _ := access.Resolve(s, friend); d.Role != "" {
		t.Fatalf("revoked: %+v", d)
	}
}

func TestLendOnAnAdminIsRefused(t *testing.T) {
	s := store(t)
	knock(t, s, op, t0)
	if _, err := change(t, s, fleetapi.AccessAdmin, "op-mac.tail1234.ts.net"); err != nil {
		t.Fatal(err)
	}
	if _, err := change(t, s, fleetapi.AccessLend, "op-mac", "agent-09"); !errors.Is(err, access.ErrBadChange) || !strings.Contains(err.Error(), "admin") {
		t.Fatalf("lend on an admin: %v", err)
	}
	if d, _ := access.Resolve(s, op); d.Role != fleetapi.RoleAdmin || len(d.Agents) != 0 {
		t.Fatalf("after: %+v", d)
	}
	if _, err := change(t, s, fleetapi.AccessUnlend, "op-mac", "agent-09"); !errors.Is(err, access.ErrBadChange) {
		t.Fatalf("unlend on an admin: %v", err)
	}
	if _, err := change(t, s, "owner", "op-mac"); !errors.Is(err, access.ErrBadChange) {
		t.Fatalf("an unknown op: %v", err)
	}
}

func TestAShortNameMustMatchOneDevice(t *testing.T) {
	s := store(t)
	other := tailnet.Who{StableID: "nFRIEND2", Login: "pal@example.com", Node: "friend-laptop.tail9abc.ts.net"}
	knock(t, s, friend, t0)
	knock(t, s, other, t0)
	if _, _, _, err := access.Find(s, "friend-laptop"); !errors.Is(err, access.ErrBadChange) || !strings.Contains(err.Error(), "is 2 devices") {
		t.Fatalf("an ambiguous short name: %v", err)
	}
	for ref, want := range map[string]string{
		"friend-laptop.tail9abc.ts.net":  "nFRIEND2",
		"Friend-Laptop.tail1234.ts.net.": "nFRIEND1",
		"nFRIEND2":                       "nFRIEND2",
	} {
		if id, _, _, err := access.Find(s, ref); err != nil || id != want {
			t.Errorf("%s: %s %v", ref, id, err)
		}
	}
	if _, _, _, err := access.Find(s, "pal-desktop"); !errors.Is(err, access.ErrNoDevice) {
		t.Fatalf("an unknown device: %v", err)
	}
	if _, _, _, err := access.Find(s, "friend"); !errors.Is(err, access.ErrNoDevice) {
		t.Fatalf("a prefix is no short name: %v", err)
	}
}

func TestTheFirstKnockInADayIsTheOnlyOneTold(t *testing.T) {
	s := store(t)
	var told []string
	tell := func(text string) { told = append(told, text) }
	for _, at := range []time.Time{t0, t0.Add(time.Hour), t0.Add(23 * time.Hour), t0.Add(25 * time.Hour), t0.Add(26 * time.Hour)} {
		if err := access.Knocked(s, friend, at, tell); err != nil {
			t.Fatal(err)
		}
	}
	if len(told) != 2 || !strings.Contains(told[0], "friend-laptop (friend@example.com) asked for fleet access") {
		t.Fatalf("told: %q", told)
	}
	if err := access.Knocked(s, op, t0.Add(26*time.Hour), tell); err != nil || len(told) != 3 {
		t.Fatalf("another device: %q %v", told, err)
	}
}
