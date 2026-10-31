package googleacct

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/MelloB1989/karmax/internal/tools"
)

func toolNamed(t *testing.T, set []tools.Tool, name string) tools.Tool {
	t.Helper()
	for _, tl := range set {
		if tl.Manifest().Name == name {
			if !json.Valid(tl.Manifest().Parameters) {
				t.Fatalf("%s: parameters are not valid JSON", name)
			}
			return tl
		}
	}
	t.Fatalf("no tool %s", name)
	return nil
}

func twoAccounts() (*fakeCLI, *Client) {
	f := &fakeCLI{}
	f.on("gog", "", "auth list", reply{out: `{"accounts":[{"email":"work@example.org","services":["calendar","chat"],"auth":"oauth"}]}`})
	f.on("gws", "", "auth status", reply{out: gwsStatus(acctAddr, true)})
	return f, NewClient(hostWith(f, "/cfg/gws"), time.UTC)
}

func run(t *testing.T, tl tools.Tool, in map[string]any) tools.ToolResult {
	t.Helper()
	res, err := tl.Execute(context.Background(), in)
	if err != nil {
		t.Fatalf("%s: %v", tl.Manifest().Name, err)
	}
	return res
}

// With more than one account, an action never guesses which one it is.
func TestAnActionNamesItsAccountWhenThereIsAChoice(t *testing.T) {
	f, c := twoAccounts()
	f.on("gws", "", "chat spaces messages create", reply{out: `{"name":"spaces/S1/messages/r"}`})
	send := toolNamed(t, Tools(c, time.UTC, ""), "gchat.send")
	res := run(t, send, map[string]any{"space": "spaces/S1", "text": "hi"})
	if !res.IsError || !strings.Contains(res.Error, "work@example.org") || !strings.Contains(res.Error, acctAddr) {
		t.Fatalf("an ambiguous account should be refused with the choices: %+v", res)
	}
	if res := run(t, send, map[string]any{"account": acctAddr, "space": "spaces/S1", "text": "hi"}); res.IsError {
		t.Fatalf("send as a named account: %v", res.Error)
	}
	if got := f.find(t, "gws", "chat", "messages", "create"); got.envValue("GOOGLE_WORKSPACE_CLI_CONFIG_DIR") != "/cfg/gws" {
		t.Errorf("sent through the wrong login: %v", got)
	}
}

// Lent for one account's event, the tools act as that account unless told
// otherwise.
func TestLentToolsActAsTheEventsAccount(t *testing.T) {
	f, c := twoAccounts()
	f.on("gog", "", "calendar respond primary ev1", reply{out: `{"event":{"id":"ev1"}}`})
	respond := toolNamed(t, Tools(c, time.UTC, "work@example.org"), "gcal.respond")
	res := run(t, respond, map[string]any{"event_id": "ev1", "response": "accepted"})
	if res.IsError {
		t.Fatalf("respond: %v", res.Error)
	}
	if got := f.find(t, "gog", "calendar", "respond"); argValue(got.args, "--account") != "work@example.org" {
		t.Errorf("answered as the wrong account: %v", got)
	}
	if out, _ := json.Marshal(res.Output); !strings.Contains(string(out), "work@example.org") {
		t.Errorf("the result should say which account acted: %s", out)
	}
}

func TestRescheduleAndCreateReadTimesInTheOperatorsZone(t *testing.T) {
	f, c := gwsOnly()
	f.on("gws", "", "calendar events insert", reply{out: `{"id":"n1"}`})
	ist := time.FixedZone("IST", 5*3600+1800)
	create := toolNamed(t, Tools(c, ist, ""), "gcal.create")
	res := run(t, create, map[string]any{"title": "Lunch with Anita", "start": "2026-11-05T13:00", "attendees": []any{"anita@example.com"}})
	if res.IsError {
		t.Fatalf("create: %v", res.Error)
	}
	b := body(t, f.find(t, "gws", "calendar", "events", "insert"))
	if got := b["start"].(map[string]any)["dateTime"]; got != "2026-11-05T13:00:00+05:30" {
		t.Errorf("a time without a zone is the operator's: %v", got)
	}
	if res := run(t, create, map[string]any{"title": "x", "start": "next tuesday"}); !res.IsError {
		t.Error("an unreadable time was accepted")
	}

	f.on("gws", "", "calendar events patch", reply{out: `{"id":"e1"}`})
	re := toolNamed(t, Tools(c, ist, ""), "gcal.reschedule")
	res = run(t, re, map[string]any{"event_id": "e1", "start": "2026-11-05T15:00:00+05:30", "end": "2026-11-05T15:30:00+05:30", "notify": false})
	if res.IsError {
		t.Fatalf("reschedule: %v", res.Error)
	}
	if p := params(t, f.find(t, "gws", "calendar", "events", "patch")); p["sendUpdates"] != "none" {
		t.Errorf("notify:false should not email the guests: %v", p)
	}
}

func TestSendNeedsASpaceOrAnAddress(t *testing.T) {
	_, c := gwsOnly()
	send := toolNamed(t, Tools(c, time.UTC, ""), "gchat.send")
	if res := run(t, send, map[string]any{"text": "hi"}); !res.IsError {
		t.Fatal("a message with nowhere to go was accepted")
	}
}

func TestAccountsSaysHowEachIsReachedAndWhatToRunWhenItIsNot(t *testing.T) {
	f := &fakeCLI{}
	f.on("gog", "", "auth list", reply{out: `{"accounts":[]}`})
	f.on("gws", "/cfg/gws", "auth status", reply{out: gwsStatus(acctAddr, true)})
	f.on("gws", "/cfg/gws-side", "auth status", reply{out: gwsStatus("dev@example.net", false)})
	c := NewClient(hostWith(f, "/cfg/gws", "/cfg/gws-side"), time.UTC)
	res := run(t, toolNamed(t, Tools(c, time.UTC, ""), "google.accounts"), map[string]any{})
	out, _ := json.Marshal(res.Output)
	s := string(out)
	if !strings.Contains(s, acctAddr) || !strings.Contains(s, "dev@example.net") || !strings.Contains(s, "gws auth login") {
		t.Fatalf("accounts output %s", s)
	}
}
