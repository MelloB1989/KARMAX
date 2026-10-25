package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/MelloB1989/karmax/fleet/internal/access"
	"github.com/MelloB1989/karmax/fleet/internal/config"
	"github.com/MelloB1989/karmax/fleet/internal/daemon"
	"github.com/MelloB1989/karmax/fleet/internal/fleetapi"
	"github.com/MelloB1989/karmax/fleet/internal/identity"
	"github.com/MelloB1989/karmax/fleet/internal/ledger"
)

// The fake's access: agent-09 and agent-10 are sandboxes.
func (f *fake) AccessList() (fleetapi.AccessList, error) {
	f.calls = append(f.calls, "access")
	return fleetapi.AccessList{Devices: []fleetapi.Device{}, Knocks: []fleetapi.Knock{}}, nil
}
func (f *fake) ChangeAccess(c fleetapi.AccessChange, by string, _ time.Time) (fleetapi.AccessList, error) {
	switch {
	case c.Device == "nobody":
		return fleetapi.AccessList{}, fmt.Errorf("%w %q", access.ErrNoDevice, c.Device)
	case c.Op == "owner":
		return fleetapi.AccessList{}, fmt.Errorf("%w: op %q", access.ErrBadChange, c.Op)
	}
	f.calls = append(f.calls, fmt.Sprintf("access %s %s %s by %s", c.Op, c.Device, strings.Join(c.Agents, ","), by))
	return fleetapi.AccessList{Devices: []fleetapi.Device{}, Knocks: []fleetapi.Knock{}}, nil
}
func (f *fake) KnockCount() int { return 0 }
func (f *fake) Sandbox(w string) bool {
	b := identity.Base(w)
	return b == "agent-09" || b == "agent-10"
}
func (f *fake) PickAmong(repo string, among []string, _ time.Time) daemon.PickReply {
	f.calls = append(f.calls, "pickamong "+strings.Join(among, ","))
	return daemon.PickReply{Agent: among[0], Candidates: []daemon.Candidate{{Agent: among[0], Eligible: true}}}
}

// guestAllowed is every route a guest may reach. Each one filters what it
// answers to the guest's lent agents and own tasks, or checks them; every
// other route refuses a guest. The TUI's routes are here before they merge.
var guestAllowed = []string{
	"GET /v1", "GET /v1/whoami", "GET /v1/guide",
	"POST /v1/run", "GET /v1/pick", "GET /v1/wait", "POST /v1/done",
	"GET /v1/tasks", "GET /v1/tasks/{id}", "POST /v1/tasks/{id}/followup", "POST /v1/tasks/{id}/cancel",
	"POST /v1/tasks/{id}/answer", "POST /v1/tasks/{id}/brief",
	"GET /v1/peek", "PUT /v1/agents/{agent}/files", "GET /v1/agents/{agent}/files", "POST /v1/agents/{agent}/rsh",
	"GET /v1/watch", "GET /v1/screens", "GET /v1/term/{worker}",
	"GET /v1/events", "GET /v1/status", "GET /v1/quota", "GET /v1/usage", "GET /v1/activity", "GET /v1/today",
}

// noLendsIP is a guest device with nothing lent.
const noLendsIP = "100.64.0.8"

// routeIndex is GET /v1 as the full token reads it.
func routeIndex(t *testing.T, h http.Handler) []Route {
	t.Helper()
	rec := do(t, h, "GET", "/v1", "full", "")
	var idx []Route
	if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &idx) != nil || len(idx) < 30 {
		t.Fatalf("index: %d %s", rec.Code, rec.Body)
	}
	return idx
}

// pathValue fills a route's wildcards with what is not a guest's: the
// operator's agent and task.
var pathValue = regexp.MustCompile(`\{[a-z]+(\.\.\.)?\}`)

// A guest reaches nothing beyond its lends through any route, listed or
// added later: the walk is over the router's own index, so a new route with
// no rule for guests fails here.
func TestEveryRouteHasARuleForAGuest(t *testing.T) {
	f, api := setup(t)
	d := tagDevices(t)
	nobody := tailnetWho[outIP]
	nobody.StableID = "nNOLENDS"
	who := fakeWho{}
	for ip, w := range tailnetWho {
		who[ip] = w
	}
	who[noLendsIP] = nobody
	if err := d.db.Tag(fleetapi.Device{StableID: nobody.StableID, Name: nobody.Node, Login: nobody.Login, Role: fleetapi.RoleGuest, TaggedAt: time.Now(), TaggedBy: "operator"}); err != nil {
		t.Fatal(err)
	}
	h := Tailnet(api, who, d, &config.Tailnet{Listen: "100.64.0.1:7878", GuestOpen: 2, Dist: t.TempDir()})
	foreign := []string{"agent-03", "agent-02", `"T1"`, "T-ram"} // the operator's, and another guest's
	for _, rt := range routeIndex(t, api) {
		if len(rt.Scopes) == 0 {
			t.Errorf("%s: no scope may use it", rt.Route)
		}
		allowed := slices.Contains(guestAllowed, rt.Route)
		if slices.Contains(rt.Scopes, string(guest)) && !allowed {
			t.Errorf("%s: open to guests, but not in guestAllowed: give it a lend check and list it", rt.Route)
		}
		method, path, _ := strings.Cut(rt.Route, " ")
		path = pathValue.ReplaceAllStringFunc(path, func(w string) string {
			if strings.Contains(w, "id") {
				return "T1"
			}
			return "agent-03"
		})
		path += "?agent=agent-03&task=T1&workers=agent-03&timeout=1s"
		body := `{"brief":"x","agent":"agent-03","task":"T1","text":"x","op":"admin","device":"friend-laptop"}`
		for _, ip := range []string{friendIP, noLendsIP} {
			f.calls = nil
			rec := tdo(t, h, method, path, ip, "", body)
			refused := rec.Code == 403 || rec.Code == 404 || rec.Code == 400
			switch {
			case !allowed && rec.Code != 403 && rec.Code != 404:
				t.Errorf("%s as a guest (%s): %d %s", rt.Route, ip, rec.Code, rec.Body)
			case !allowed && len(f.calls) > 0:
				t.Errorf("%s as a guest reached the fleet: %v", rt.Route, f.calls)
			case allowed && !refused && rec.Code != 200:
				t.Errorf("%s as a guest (%s): %d %s", rt.Route, ip, rec.Code, rec.Body)
			case allowed && !refused && rt.Route != "GET /v1" && rt.Route != "GET /v1/guide":
				for _, x := range foreign {
					if strings.Contains(rec.Body.String(), x) {
						t.Errorf("%s shows a guest %s: %s", rt.Route, x, rec.Body)
					}
				}
			}
		}
	}
}

// guestOps is the fake with a fleet that has a sandbox, agent-09, beside the
// operator's agents, and data on each.
type guestOps struct{ *fake }

func (guestOps) Status(time.Time) []daemon.Row {
	return []daemon.Row{{Agent: "agent-03", Account: "acct-op", Task: "T1"}, {Agent: "agent-09", Account: "acct-09", Task: "T-friend"},
		{Agent: "agent-09:2", Account: "acct-09"}}
}
func (guestOps) Events(daemon.EventsQuery) ([]ledger.Event, error) {
	return []ledger.Event{
		{ID: 1, Agent: "agent-03", Kind: "Stop"},
		{ID: 2, Agent: "agent-09", Kind: "fleet.assign", Detail: `{"task":"T-friend"}`},
		{ID: 3, Agent: "agent-09", Kind: "fleet.assign", Detail: `{"task":"T-pal"}`}, // another guest's, on the same lent agent
		{ID: 4, Agent: "agent-09:2", Kind: "otel.tool_result"},
		{ID: 5, Agent: "", Kind: "fleet.access.lend"},
	}, nil
}
func (guestOps) Quota() (map[string]ledger.Quota, error) {
	return map[string]ledger.Quota{"acct-op": {Account: "acct-op"}, "acct-09": {Account: "acct-09"}}, nil
}
func (guestOps) Usage(string, time.Time) ([]ledger.UsageRow, error) {
	return []ledger.UsageRow{{Key: "agent-03"}, {Key: "agent-09:2"}}, nil
}
func (g guestOps) TaskAgent(task string) (*ledger.Task, error) {
	switch task {
	case "T-friend":
		return &ledger.Task{ID: task, Agent: "agent-09", Assigner: "friend@example.com/cc-1"}, nil
	case "T-pal":
		return &ledger.Task{ID: task, Agent: "agent-09", Assigner: "pal@example.com"}, nil
	case "T-unlent":
		return &ledger.Task{ID: task, Agent: "agent-10", Assigner: "friend@example.com"}, nil
	}
	return g.fake.TaskAgent(task)
}
func (g guestOps) Activity(since time.Time, b time.Duration, now time.Time) (daemon.ActivityReply, error) {
	return daemon.ActivityReply{Agents: map[string][]ledger.Bucket{"agent-03": {{N: 1}}, "agent-09:2": {{N: 2}}}}, nil
}

func guestSetup(t *testing.T) (*fake, http.Handler) {
	t.Helper()
	f := &fake{}
	api := Handler(guestOps{f}, Tokens{Full: "full", Relay: "relay", Console: "console"}, nil)
	return f, Tailnet(api, tailnetWho, tagDevices(t), &config.Tailnet{Listen: "100.64.0.1:7878", GuestOpen: 2, Dist: t.TempDir()})
}

func TestAGuestSeesOnlyItsLentAgentsAndOwnTasks(t *testing.T) {
	f, h := guestSetup(t)
	get := func(path string, v any) {
		t.Helper()
		rec := tdo(t, h, "GET", path, friendIP, "", "")
		if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), v) != nil {
			t.Fatalf("%s: %d %s", path, rec.Code, rec.Body)
		}
	}
	var st struct{ Agents []daemon.Row }
	get("/v1/status", &st)
	if len(st.Agents) != 2 || st.Agents[0].Agent != "agent-09" || st.Agents[1].Agent != "agent-09:2" {
		t.Errorf("status: %+v", st.Agents)
	}
	var evs []ledger.Event
	get("/v1/events", &evs)
	if ids := fmt.Sprint(slices.Collect(func(yield func(int64) bool) {
		for _, e := range evs {
			yield(e.ID)
		}
	})); ids != "[2 4]" {
		t.Errorf("events: %s", ids)
	}
	if rec := tdo(t, h, "GET", "/v1/events?agent=agent-03", friendIP, "", ""); rec.Code != 403 {
		t.Errorf("events of another agent: %d", rec.Code)
	}
	var qs map[string]ledger.Quota
	get("/v1/quota", &qs)
	if _, ok := qs["acct-09"]; !ok || len(qs) != 1 {
		t.Errorf("quota: %+v", qs)
	}
	var us []ledger.UsageRow
	get("/v1/usage?by=model", &us)
	if len(us) != 1 || us[0].Key != "agent-09:2" {
		t.Errorf("usage: %+v", us)
	}
	var act daemon.ActivityReply
	get("/v1/activity", &act)
	if _, ok := act.Agents["agent-09:2"]; !ok || len(act.Agents) != 1 {
		t.Errorf("activity: %+v", act)
	}
	f.calls = nil
	get("/v1/tasks?open=1", &[]ledger.Task{})
	if !slices.Contains(f.calls, `tasks open=true agent="" n=0 by=friend@example.com`) {
		t.Errorf("tasks: %v", f.calls)
	}
	if rec := tdo(t, h, "GET", "/v1/today", friendIP, "", ""); rec.Code != 403 {
		t.Errorf("today, the whole fleet's: %d", rec.Code)
	}
	var who map[string]string
	get("/v1/whoami", &who)
	if who["scope"] != "guest" || who["lent"] != "agent-09" || who["device"] != "friend-laptop.tail1234.ts.net" {
		t.Errorf("whoami: %v", who)
	}
	// Its own task's agent it may use; while that agent works on another
	// guest's task, not.
	if rec := tdo(t, h, "GET", "/v1/peek?agent=agent-09", friendIP, "", ""); rec.Code != 200 {
		t.Errorf("peek its own task's agent: %d %s", rec.Code, rec.Body)
	}
	if rec := tdo(t, h, "GET", "/v1/peek?agent=agent-03", friendIP, "", ""); rec.Code != 403 {
		t.Errorf("peek another agent: %d", rec.Code)
	}
}

// A guest's own task on an agent no longer lent to it may be read and
// closed, but nothing more is sent to that agent.
func TestAGuestSendsNothingToAnAgentNoLongerLent(t *testing.T) {
	f, h := guestSetup(t)
	for _, c := range []struct {
		path, body string
		want       int
	}{
		{"/v1/tasks/T-unlent/followup", `{"text":"more"}`, 403},
		{"/v1/tasks/T-unlent/answer", `{"text":"yes"}`, 403},
		{"/v1/tasks/T-unlent/brief", `{"brief":"x"}`, 403},
		{"/v1/tasks/T-unlent/cancel", `{}`, 200},
		{"/v1/tasks/T-friend/followup", `{"text":"more"}`, 200},
	} {
		if rec := tdo(t, h, "POST", c.path, friendIP, "", c.body); rec.Code != c.want {
			t.Errorf("%s: %d %s", c.path, rec.Code, rec.Body)
		}
	}
	for _, c := range f.calls {
		if strings.Contains(c, "T-unlent") && !strings.HasPrefix(c, "cancel") {
			t.Errorf("reached the agent: %s", c)
		}
	}
}

// An agent lent to two devices serves one guest at a time: the other may not
// copy files with it, type into it, or read its screen meanwhile.
func TestAGuestMayNotTouchALentAgentAtWorkForAnotherGuest(t *testing.T) {
	f := &fake{}
	api := Handler(guestOps{f}, Tokens{Full: "full"}, nil)
	d := tagDevices(t)
	pal := tailnetWho[friendIP]
	pal.StableID, pal.Login, pal.Node = "nPAL", "pal@example.com", "pal-desktop.tail9abc.ts.net"
	who := fakeWho{noLendsIP: pal}
	if err := d.db.Tag(fleetapi.Device{StableID: "nPAL", Name: pal.Node, Login: pal.Login, Role: fleetapi.RoleGuest, Agents: []string{"agent-09"}, TaggedAt: time.Now(), TaggedBy: "operator"}); err != nil {
		t.Fatal(err)
	}
	h := Tailnet(api, who, d, &config.Tailnet{Listen: "100.64.0.1:7878", GuestOpen: 2})
	for _, c := range []struct{ method, path string }{
		{"GET", "/v1/peek?agent=agent-09"},
		{"PUT", "/v1/agents/agent-09/files"},
		{"GET", "/v1/agents/agent-09:2/files?path=/work/x"},
		{"POST", "/v1/agents/agent-09/rsh?arg=true"},
	} {
		if rec := tdo(t, h, c.method, c.path, noLendsIP, "", ""); rec.Code != 403 || !strings.Contains(rec.Body.String(), "not yours") {
			t.Errorf("%s %s: %d %s", c.method, c.path, rec.Code, rec.Body)
		}
	}
	if len(f.calls) != 0 {
		t.Fatalf("reached the fleet: %v", f.calls)
	}
}

func TestAGuestRunsOnlyOnItsLentAgents(t *testing.T) {
	f, h, _ := tailnetSetup(t)
	if rec := tdo(t, h, "POST", "/v1/run", friendIP, "", `{"brief":"x","agent":"agent-03"}`); rec.Code != 403 || !strings.Contains(rec.Body.String(), "agent-03 is not lent to this device") {
		t.Fatalf("an agent outside its lends: %d %s", rec.Code, rec.Body)
	}
	if rec := tdo(t, h, "POST", "/v1/run", friendIP, "", `{"brief":"x","agent":"agent-09:2","model":"opus"}`); rec.Code != 200 {
		t.Fatalf("a slot of its lent agent, its model chosen: %d %s", rec.Code, rec.Body)
	}
	if rec := tdo(t, h, "POST", "/v1/run", friendIP, "", `{"brief":"x"}`); rec.Code != 200 {
		t.Fatalf("picked: %d %s", rec.Code, rec.Body)
	}
	if len(f.runs) != 2 {
		t.Fatalf("runs: %+v", f.runs)
	}
	for _, in := range f.runs {
		if !slices.Equal(in.Among, []string{"agent-09"}) || in.AdminWork || in.Limit != 2 || in.From != "friend@example.com" {
			t.Errorf("a guest's run: %+v", in)
		}
	}
	if f.runs[0].Model != "opus" {
		t.Errorf("model: %q", f.runs[0].Model)
	}
	f.calls = nil
	if rec := tdo(t, h, "GET", "/v1/pick?repo=r", friendIP, "", ""); rec.Code != 200 || !strings.Contains(rec.Body.String(), "agent-09") || !slices.Equal(f.calls, []string{"pickamong agent-09"}) {
		t.Fatalf("pick: %d %s %v", rec.Code, rec.Body, f.calls)
	}
	// The full scope's runs, here and from an admin device, are admin work.
	f.runs = nil
	if rec := tdo(t, h, "POST", "/v1/run", opIP, "", `{"brief":"x","agent":"agent-03"}`); rec.Code != 200 {
		t.Fatalf("admin device: %d %s", rec.Code, rec.Body)
	}
	if len(f.runs) != 1 || !f.runs[0].AdminWork || f.runs[0].Among != nil || f.runs[0].Limit != 0 {
		t.Fatalf("an admin's run: %+v", f.runs)
	}
	f2, api2 := setup(t)
	if rec := do(t, api2, "POST", "/v1/run", "full", `{"brief":"x"}`); rec.Code != 200 || len(f2.runs) != 1 || !f2.runs[0].AdminWork {
		t.Fatalf("the full token's run: %d %+v", rec.Code, f2.runs)
	}
}

func TestAGuestWithNothingLentRunsNothing(t *testing.T) {
	f, api := setup(t)
	d := tagDevices(t)
	w := tailnetWho[outIP]
	if err := d.db.Tag(fleetapi.Device{StableID: w.StableID, Name: w.Node, Login: w.Login, Role: fleetapi.RoleGuest, TaggedAt: time.Now(), TaggedBy: "operator"}); err != nil {
		t.Fatal(err)
	}
	h := Tailnet(api, tailnetWho, d, &config.Tailnet{Listen: "100.64.0.1:7878", GuestOpen: 2})
	if rec := tdo(t, h, "POST", "/v1/run", outIP, "", `{"brief":"x"}`); rec.Code != 403 || !strings.Contains(rec.Body.String(), "no agents are lent") {
		t.Fatalf("run: %d %s", rec.Code, rec.Body)
	}
	if len(f.runs) != 0 {
		t.Fatalf("ran: %+v", f.runs)
	}
}

func TestAnAdminDeviceIsTheFullScope(t *testing.T) {
	f, h, _ := tailnetSetup(t)
	var who map[string]string
	rec := tdo(t, h, "GET", "/v1/whoami", opIP, "", "")
	if json.Unmarshal(rec.Body.Bytes(), &who) != nil || who["scope"] != "full" || who["role"] != "admin" || who["name"] != "op@example.com" {
		t.Fatalf("whoami: %s", rec.Body)
	}
	for _, c := range []struct{ method, path, body string }{
		{"POST", "/v1/assign", `{"agent":"agent-03","task":"T2"}`},
		{"GET", "/v1/access", ""},
		{"POST", "/v1/access", `{"op":"lend","device":"friend-laptop","agents":["agent-09"]}`},
		{"GET", "/v1/memory/recall?q=deploys", ""},
		{"GET", "/v1/inbox", ""},
		{"GET", "/v1/today", ""},
		{"GET", "/v1/peek?agent=agent-02", ""},
	} {
		if rec := tdo(t, h, c.method, c.path, opIP, "", c.body); rec.Code != 200 {
			t.Errorf("%s %s: %d %s", c.method, c.path, rec.Code, rec.Body)
		}
	}
	if !slices.Contains(f.calls, "access lend friend-laptop agent-09 by op@example.com (op-laptop.tail1234.ts.net)") {
		t.Errorf("an admin's change is not theirs: %v", f.calls)
	}
}

// A login is not admin by itself: the admin's login, and even the admin
// device's name, on another device gets nothing, and knocks.
func TestALoginAloneIsNotAdmin(t *testing.T) {
	f, h, _, d := tailnetDevices(t)
	for _, path := range []string{"/v1/whoami", "/v1/status", "/v1/access", "/v1/memory/recall?q=x"} {
		if rec := tdo(t, h, "GET", path, evilIP, "", ""); rec.Code != 403 || !strings.Contains(rec.Body.String(), "may not use this fleet yet") {
			t.Errorf("%s: %d %s", path, rec.Code, rec.Body)
		}
	}
	if len(f.calls) != 0 {
		t.Fatalf("reached the fleet: %v", f.calls)
	}
	ks, _ := d.db.Knocks(0)
	if len(ks) != 1 || ks[0].StableID != "nEVIL" || ks[0].Count != 4 {
		t.Fatalf("knocks: %+v", ks)
	}
}

func TestAnUntaggedDeviceIsToldHowToAsk(t *testing.T) {
	_, h, _ := tailnetSetup(t)
	rec := tdo(t, h, "GET", "/v1/status", outIP, "", "")
	if rec.Code != 403 || !strings.Contains(rec.Body.String(), "fleetctl access lend stranger-laptop") {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
}

// With the access store unreadable the tailnet is shut; loopback with the
// full token still works, so the operator can repair it.
func TestAnUnreadableAccessStoreDeniesTheTailnet(t *testing.T) {
	_, api := setup(t)
	d := tagDevices(t)
	d.broken = true
	h := Tailnet(api, tailnetWho, d, &config.Tailnet{Listen: "100.64.0.1:7878"})
	for _, ip := range []string{opIP, friendIP} {
		if rec := tdo(t, h, "GET", "/v1/whoami", ip, "", ""); rec.Code != 503 {
			t.Errorf("%s: %d", ip, rec.Code)
		}
	}
	if rec := do(t, api, "GET", "/v1/access", "full", ""); rec.Code != 200 {
		t.Errorf("loopback: %d", rec.Code)
	}
}

func TestAccessIsForAdminsOnly(t *testing.T) {
	f, h, _ := tailnetSetup(t)
	_, api := setup(t)
	for _, c := range []struct {
		method, body, token, ip string
		want                    int
	}{
		{"GET", "", "full", "", 200},
		{"POST", `{"op":"revoke","device":"friend-laptop"}`, "full", "", 200},
		{"POST", `{"op":"owner","device":"friend-laptop"}`, "full", "", 400},
		{"POST", `{"op":"admin","device":"nobody"}`, "full", "", 404},
		{"GET", "", "relay", "", 403},
		{"GET", "", "console", "", 403},
		{"POST", `{"op":"admin","device":"ram-laptop"}`, "", ramIP, 403},
		{"GET", "", "", friendIP, 403},
	} {
		var rec *httptest.ResponseRecorder
		if c.ip != "" {
			rec = tdo(t, h, c.method, "/v1/access", c.ip, "", c.body)
		} else {
			rec = do(t, api, c.method, "/v1/access", c.token, c.body)
		}
		if rec.Code != c.want {
			t.Errorf("%s %s %s%s: %d %s", c.method, c.body, c.token, c.ip, rec.Code, rec.Body)
		}
	}
	for _, c := range f.calls {
		if strings.HasPrefix(c, "access") {
			t.Errorf("a guest reached access: %s", c)
		}
	}
}

// operatorWork asks the task, not who assigned it: admin work (the full
// scope's, on an ordinary agent) is let in; a guest's task, and any task on
// a sandbox, are not.
func TestOperatorWorkNeedsAnAdminAssignedTaskOnAnOrdinaryAgent(t *testing.T) {
	f := &fake{}
	h := Handler(sandboxOps{f}, Tokens{Full: "full", Relay: "relay"}, nil)
	for _, c := range []struct {
		agent string
		want  int
	}{
		{"agent-02", 200}, // T1: the operator's, admin work
		{"agent-03", 403}, // T-ram: a guest's
		{"agent-09", 403}, // T9: admin work, but on a sandbox
		{"agent-05", 403}, // no task
	} {
		if rec := agentDo(t, h, "GET", "/v1/memory/recall?q=x", c.agent, ""); rec.Code != c.want {
			t.Errorf("%s: %d %s", c.agent, rec.Code, rec.Body)
		}
	}
}

// sandboxOps is the fake with agent-09, a sandbox, at work on an admin's task.
type sandboxOps struct{ *fake }

func (sandboxOps) Status(time.Time) []daemon.Row {
	return []daemon.Row{{Agent: "agent-03", Task: "T-ram"}, {Agent: "agent-02", Task: "T1"}, {Agent: "agent-09", Task: "T9"}}
}
