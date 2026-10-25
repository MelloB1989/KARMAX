package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/MelloB1989/karmax/fleet/internal/fleetapi"
)

const accessReply = `{"devices":[` +
	`{"stable_id":"nOP","name":"op-mac.tail1234.ts.net","login":"op@example.com","role":"admin","tagged_at":"2026-10-25T10:00:00Z","tagged_by":"operator","last_seen":"2026-10-25T11:58:00Z"},` +
	`{"stable_id":"nF","name":"friend-laptop.tail1234.ts.net","login":"friend@example.com","role":"guest","agents":["agent-09","agent-10"],"tagged_at":"2026-10-25T10:00:00Z","tagged_by":"operator"}],` +
	`"knocks":[{"stable_id":"nP","name":"pal-desktop.tail9abc.ts.net","login":"pal@example.com","first":"2026-10-25T09:00:00Z","last":"2026-10-25T09:00:00Z","count":3}]}`

func TestAccessListsDevicesThenKnocks(t *testing.T) {
	f := &fakeFleetd{reply: map[string]string{"/v1/access": accessReply}}
	out, err := fleetctl(t, f, "access")
	if err != nil {
		t.Fatal(err)
	}
	if f.reqs[0] != "GET /v1/access " {
		t.Fatalf("request = %q", f.reqs[0])
	}
	lines := strings.Split(out, "\n")
	if len(lines) < 7 || strings.Join(strings.Fields(lines[0]), " ") != "DEVICE LOGIN ROLE AGENTS SEEN" {
		t.Fatalf("output:\n%s", out)
	}
	if got := strings.Fields(lines[1]); len(got) < 4 || got[0] != "op-mac.tail1234.ts.net" || got[2] != "admin" || got[3] != "all" {
		t.Errorf("admin row: %q", lines[1])
	}
	if got := strings.Fields(lines[2]); len(got) != 5 || got[3] != "agent-09,agent-10" || got[4] != "-" {
		t.Errorf("guest row: %q", lines[2])
	}
	if !strings.Contains(out, "Asked, not tagged") || !strings.Contains(out, "pal-desktop.tail9abc.ts.net") || !strings.Contains(out, "3×") {
		t.Errorf("knocks:\n%s", out)
	}
	if strings.Contains(out, "Claude login") {
		t.Error("the lend caveat on a listing")
	}
}

func TestAccessChangesSayTheCaveatAfterALend(t *testing.T) {
	f := &fakeFleetd{reply: map[string]string{"/v1/access": accessReply}}
	out, err := fleetctl(t, f, "access", "lend", "friend-laptop", "agent-09", "agent-10")
	if err != nil {
		t.Fatal(err)
	}
	in := reqBody(t, f.reqs[0])
	if !strings.HasPrefix(f.reqs[0], "POST /v1/access ") || in["op"] != "lend" || in["device"] != "friend-laptop" || len(in["agents"].([]any)) != 2 {
		t.Fatalf("request = %q", f.reqs[0])
	}
	if !strings.Contains(out, "friend-laptop.tail1234.ts.net") || !strings.Contains(out, "can read its Claude login") {
		t.Fatalf("output:\n%s", out)
	}
	for _, args := range [][]string{{"admin", "op-mac"}, {"unlend", "friend-laptop", "agent-10"}, {"revoke", "friend-laptop"}} {
		out, err := fleetctl(t, f, append([]string{"access"}, args...)...)
		if err != nil || strings.Contains(out, "Claude login") {
			t.Errorf("%v: %v\n%s", args, err, out)
		}
	}
	for _, bad := range [][]string{{"lend", "friend-laptop"}, {"admin", "op-mac", "agent-09"}, {"owner", "op-mac"}, {"revoke"}} {
		if _, err := fleetctl(t, f, append([]string{"access"}, bad...)...); err == nil || !strings.Contains(err.Error(), "usage") {
			t.Errorf("%v: %v", bad, err)
		}
	}
}

func TestAnEmptyAccessListSaysWhoMayUseTheFleet(t *testing.T) {
	var b bytes.Buffer
	printAccess(&b, fleetapi.AccessList{}, time.Now())
	if !strings.Contains(b.String(), "no tagged devices") {
		t.Fatalf("%s", b.String())
	}
}
