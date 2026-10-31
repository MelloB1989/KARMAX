package runtime

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MelloB1989/karmax/internal/bus"
	"github.com/MelloB1989/karmax/internal/integration"
	"github.com/MelloB1989/karmax/internal/sources/googleacct"
	"github.com/MelloB1989/karmax/pkg/connectorkit"
	"go.uber.org/zap"
)

type mapKV struct {
	mu sync.Mutex
	m  map[string]string
}

func (k *mapKV) KVGet(g, key string) (string, bool, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	v, ok := k.m[g+"\x00"+key]
	return v, ok, nil
}

func (k *mapKV) KVSet(g, key, v string, _ time.Duration) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.m[g+"\x00"+key] = v
	return nil
}

// googleRig is one gws login for priya@example.com whose answers the test
// sets per command.
type googleRig struct {
	mu        sync.Mutex
	answers   map[string]string // command words -> stdout
	failAuth  bool
	published []bus.Event
	regs      map[string]integration.Integration
	g         *googleSources
}

func newGoogleRig(t *testing.T, getenv func(string) string) *googleRig {
	r := &googleRig{answers: map[string]string{}, regs: map[string]integration.Integration{}}
	run := func(_ context.Context, _ []string, bin string, args ...string) ([]byte, []byte, int, error) {
		r.mu.Lock()
		defer r.mu.Unlock()
		line := bin + " " + strings.Join(args, " ")
		if strings.Contains(line, "auth list") {
			return []byte(`{"accounts":[]}`), nil, 0, nil
		}
		if strings.Contains(line, "auth status") {
			return []byte(`{"user":"priya@example.com","has_refresh_token":true,"scopes":["https://www.googleapis.com/auth/calendar","https://www.googleapis.com/auth/chat.messages"]}`), nil, 0, nil
		}
		if r.failAuth {
			return nil, []byte("invalid_grant: Token has been expired or revoked."), 2, fmt.Errorf("exit status 2")
		}
		for words, out := range r.answers {
			match := true
			for _, w := range strings.Fields(words) {
				if !strings.Contains(line, w) {
					match = false
					break
				}
			}
			if match {
				return []byte(out), nil, 0, nil
			}
		}
		return nil, []byte("no answer for " + line), 1, fmt.Errorf("exit status 1")
	}
	host := &googleacct.Host{Gog: "gog", Gws: "gws", GwsDirs: func() []string { return []string{"/cfg/gws"} },
		HasLogin: func(string) bool { return true }, Run: run, TTL: time.Nanosecond}
	r.g = newGoogleSources(googleSourceDeps{
		Host: host, KV: &mapKV{m: map[string]string{}}, AgentID: "nexus", Loc: time.UTC, Log: zap.NewNop(), Getenv: getenv,
		Publish:  func(e bus.Event) error { r.mu.Lock(); r.published = append(r.published, e); r.mu.Unlock(); return nil },
		Register: func(in integration.Integration) { r.regs[in.Manifest().ID] = in },
	})
	return r
}

func noEnv(string) string { return "" }

func TestTheCalendarSourcePublishesWebhookEventsForEachAccount(t *testing.T) {
	r := newGoogleRig(t, noEnv)
	soon := time.Now().Add(10 * time.Minute).UTC().Format(time.RFC3339)
	end := time.Now().Add(40 * time.Minute).UTC().Format(time.RFC3339)
	r.answers["calendar events list"] = `{"items":[{"id":"e1","status":"confirmed","summary":"Investor call","start":{"dateTime":"` + soon + `"},"end":{"dateTime":"` + end + `"}}],"nextSyncToken":"s1"}`

	if err := r.g.tick(context.Background(), "gcal"); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if len(r.published) != 1 {
		t.Fatalf("published %d events, want the starting-soon reminder", len(r.published))
	}
	e := r.published[0]
	if e.Kind != bus.EventWebhookFired || e.AgentID != "nexus" || e.Payload["route"] != "gcal:priya@example.com" || e.Payload["urgent"] != true {
		t.Fatalf("event %+v", e)
	}
	in, ok := r.regs["google_account:priya@example.com"]
	if !ok {
		t.Fatalf("the account's health was not registered: %v", r.regs)
	}
	if err := in.Health(context.Background(), connectorCreds()); err != nil {
		t.Errorf("a working account reports unhealthy: %v", err)
	}
}

// A signed-out account says so in health, with the command that fixes it, and
// is told once — not every tick.
func TestASignedOutAccountSurfacesInHealthOnce(t *testing.T) {
	r := newGoogleRig(t, noEnv)
	r.failAuth = true
	for i := 0; i < 3; i++ {
		if err := r.g.tick(context.Background(), "gcal"); err != nil {
			t.Fatalf("a signed-out account failed the loop itself: %v", err)
		}
	}
	in := r.regs["google_account:priya@example.com"]
	if in == nil {
		t.Fatal("no health entry for the account")
	}
	err := in.Health(context.Background(), connectorCreds())
	if err == nil || !strings.Contains(err.Error(), "gws auth login") {
		t.Fatalf("health should say what to run: %v", err)
	}
	var told int
	for _, e := range r.published {
		if e.Payload["type"] == "account.signed_out" {
			told++
			if e.Payload["urgent"] != true || !strings.Contains(fmt.Sprint(e.Payload["summary"]), "gws auth login") {
				t.Errorf("signed-out event %+v", e.Payload)
			}
		}
	}
	if told != 1 {
		t.Fatalf("told about the sign-out %d times, want once", told)
	}

	r.failAuth = false
	r.answers["calendar events list"] = `{"items":[],"nextSyncToken":"s2"}`
	if err := r.g.tick(context.Background(), "gcal"); err != nil {
		t.Fatal(err)
	}
	if err := in.Health(context.Background(), connectorCreds()); err != nil {
		t.Errorf("recovered account still unhealthy: %v", err)
	}
}

func TestPersonalAccountsAreNotAskedForChat(t *testing.T) {
	if !chatUnsupported("someone@gmail.com") || chatUnsupported("priya@example.com") {
		t.Fatal("Chat's API serves Workspace accounts only")
	}
}

func TestOffSwitchesAndTheWorkflowTheChatSourceReplaces(t *testing.T) {
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	if cal, chat := googleSourcesOn(noEnv); !cal || !chat {
		t.Fatal("both sources are on by default")
	}
	if cal, chat := googleSourcesOn(env(map[string]string{"KARMAX_SOURCE_GCAL": "off", "KARMAX_SOURCE_GCHAT": "OFF"})); cal || chat {
		t.Fatal("off switches ignored")
	}
	if !supersededByNative("gchat-watch", noEnv) {
		t.Error("the gchat-watch workflow should stand down while the native Chat source runs")
	}
	if supersededByNative("gchat-watch", env(map[string]string{"KARMAX_SOURCE_GCHAT": "off"})) {
		t.Error("with the Chat source off, gchat-watch is the operator's to run")
	}
	if supersededByNative("wa-monitor", noEnv) {
		t.Error("an unrelated workflow was stood down")
	}

	r := newGoogleRig(t, env(map[string]string{"KARMAX_SOURCE_GCAL": "off"}))
	var names []string
	for _, l := range r.g.loops() {
		names = append(names, l.Name)
	}
	if strings.Join(names, ",") != "gchat-source" {
		t.Fatalf("loops %v with the calendar source off", names)
	}
}

func TestToolsAreLentForAnAccountsOwnEvents(t *testing.T) {
	r := newGoogleRig(t, noEnv)
	rt := &KarmaxRuntime{google: r.g}
	ev := bus.NewEvent(bus.EventWebhookFired, "nexus", map[string]any{"source": "gchat", "account": "priya@example.com"})
	lent := rt.googleToolsForEvent(ev)
	if len(lent) != len(googleacct.ToolNames()) {
		t.Fatalf("lent %d tools for a gchat event", len(lent))
	}
	other := bus.NewEvent(bus.EventWebhookFired, "nexus", map[string]any{"source": "github", "account": "octo"})
	if got := rt.googleToolsForEvent(other); len(got) != 0 {
		t.Fatalf("lent Google tools to a GitHub event: %d", len(got))
	}
}

func connectorCreds() connectorkit.Credentials { return connectorkit.Credentials{} }
