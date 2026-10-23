package console

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestFillMarksRemoteAgentsNotLive(t *testing.T) {
	srv := fleetdServer(t, nil)
	defer srv.Close()
	cfg := &Config{Host: "pc2", FleetdURL: srv.URL, FleetdTokenFile: writeToken(t), Orchestrator: "karmax",
		Term: TermSize{Cols: 220, Rows: 50},
		Agents: []AgentConf{
			{Name: "agent-02", Host: "kali", Container: "x", Exec: []string{"docker"}},
			{Name: "agent-01", Host: "pc2", Container: "agent-01", Exec: []string{"docker"}, Live: true},
		}}
	now := time.Date(2026, 10, 11, 14, 0, 5, 0, time.UTC)
	s := NewSources(cfg, &fakeRunner{fn: func([]string) ([]byte, error) { return nil, nil }}, func() time.Time { return now })
	ctx := context.Background()
	s.fleetd.poll(ctx)
	s.fleetd.pollToday(ctx)
	s.events.handle("agent-01", ev(now.Unix()-5, "PreToolUse", "Bash"), now)
	s.status.by["agent-01"] = StatusLine{Model: "Sonnet 5.5", ContextPct: 8, At: now.Add(-3 * time.Second)}
	s.docker.samples["agent-01"] = Container{Running: true, CPUPct: 2, At: now}
	s.docker.samples["x"] = Container{Running: true, CPUPct: 99, At: now} // not a roster-live container: must not show

	var snap Snapshot
	s.Fill(&snap)

	if len(snap.Agents) != 2 || snap.Agents[0].ID != "agent-01" || snap.Agents[1].ID != "agent-02" {
		t.Fatalf("agents must be ordered by name: %+v", snap.Agents)
	}
	a1, a2 := snap.Agents[0], snap.Agents[1]
	if !a1.Live || a1.CurrentTool == nil || a1.CurrentTool.Name != "Bash" || a1.Container == nil || a1.Container.CPUPct != 2 ||
		a1.Model != "Sonnet 5.5" || a1.Term != (TermSize{220, 50}) || a1.Phase != "working" || a1.LastTask == nil {
		t.Fatalf("live agent = %+v", a1)
	}
	if a1.StatuslineAt.IsZero() || a1.RecordedAt.IsZero() {
		t.Fatal("every group of fields carries its own at")
	}
	if a2.Live || a2.CurrentTool != nil || a2.Container != nil || a2.Term != (TermSize{}) || a2.Model != "" || !a2.LastEventAt.IsZero() {
		t.Fatalf("remote agent has live fields: %+v", a2)
	}
	// No status line reaches here from another host: its quota is fleetd's,
	// dated when fleetd knew it.
	if a2.ContextPct != 12 || a2.FiveHourPct != 40 || a2.SevenDayPct != 7 || a2.FiveHourResets.IsZero() ||
		!a2.StatuslineAt.Equal(time.Date(2026, 10, 11, 13, 59, 30, 0, time.UTC)) {
		t.Fatalf("remote agent's quota is not fleetd's: %+v", a2)
	}
	if a1.ContextPct != 8 || a1.FiveHourPct != 0 {
		t.Fatalf("a live agent's quota is its status line's, not fleetd's: %+v", a1)
	}
	if a2.Phase != "standby" || a2.Host != "kali" || a2.Alerts == nil || a2.Models == nil {
		t.Fatalf("remote agent keeps fleetd's fields: %+v", a2)
	}
	if snap.Sources[SrcFleetd].OK != true || snap.Sources[SrcTerminals].Error == "" || snap.Sources[SrcHost].OK {
		t.Fatalf("sources = %+v", snap.Sources)
	}
	if snap.Host.Name != "pc2" || snap.Orchestrator == nil || len(snap.Tasks) != 1 || snap.Today.TasksClosed != 2 || len(snap.Events) != 2 {
		t.Fatalf("snapshot = %+v", snap)
	}
	if _, ok := snap.Activity["agent-02"]; !ok {
		t.Fatal("every roster agent has an activity entry")
	}
}

func evSession(t int64, event, tool, session string) []byte {
	return []byte(strings.Replace(string(ev(t, event, tool)), `"session":"s1"`, `"session":"`+session+`"`, 1))
}

func TestEventsFromAnotherSessionStayOutOfTheStream(t *testing.T) {
	cfg := &Config{Host: "pc2", FleetdURL: "http://unused.invalid", FleetdTokenFile: "/nonexistent", Term: TermSize{Cols: 220, Rows: 50},
		Agents: []AgentConf{{Name: "agent-01", Host: "pc2", Container: "agent-01", Exec: []string{"docker"}, Live: true}}}
	now := time.Date(2026, 10, 11, 14, 0, 5, 0, time.UTC)
	s := NewSources(cfg, &fakeRunner{fn: func([]string) ([]byte, error) { return nil, nil }}, func() time.Time { return now })
	s.status.by["agent-01"] = StatusLine{SessionID: "own", At: now}

	at := now.Unix() - 10
	// the agent's own session works; a `claude -p` delivery in the same container runs and ends
	s.events.handle("agent-01", evSession(at, "UserPromptSubmit", "", "own"), now)
	s.events.handle("agent-01", evSession(at+1, "PreToolUse", "Bash", "own"), now)
	s.events.handle("agent-01", evSession(at+2, "SessionStart", "", "tell-1"), now)
	s.events.handle("agent-01", evSession(at+3, "PreToolUse", "Read", "tell-1"), now)
	s.events.handle("agent-01", evSession(at+4, "UserPromptSubmit", "", "tell-1"), now)
	s.events.handle("agent-01", evSession(at+5, "Stop", "", "tell-1"), now)
	s.events.handle("agent-01", evSession(at+6, "SessionEnd", "", "tell-1"), now)

	var snap Snapshot
	s.Fill(&snap)
	if len(snap.Events) != 1 || snap.Events[0].Kind != "UserPromptSubmit" || snap.Events[0].Session != "own" {
		t.Fatalf("stream = %+v, want only the agent's own session", snap.Events)
	}
	if tool := snap.Agents[0].CurrentTool; tool == nil || tool.Name != "Bash" {
		t.Fatalf("current tool = %+v: the other session's Read must not replace it", tool)
	}
	var n int
	for _, b := range snap.Activity["agent-01"] {
		n += b.N
	}
	if n != 7 {
		t.Fatalf("activity counted %d, want every event (7)", n)
	}
	// the other session's Stop must not have cleared the agent's tool either (checked above); and
	// with the status line gone, fleetd's row.session is the fallback — here neither: nothing is filtered
	delete(s.status.by, "agent-01")
	snap = Snapshot{}
	s.Fill(&snap)
	if len(snap.Events) != 5 {
		t.Fatalf("no known session: stream = %d events, want all 5 stream-kind ones", len(snap.Events))
	}
}

// A poll that never returns must be cut off after twice the cadence, and the
// next poll must still run.
func TestEveryBoundsEachPoll(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var calls atomic.Int32
	cut := make(chan time.Duration, 4)
	go every(ctx, 20*time.Millisecond, func(c context.Context) {
		dl, ok := c.Deadline()
		if !ok {
			t.Error("poll context has no deadline")
			return
		}
		if calls.Add(1) <= 2 {
			start := time.Now()
			<-c.Done() // a hung command
			cut <- time.Since(start)
		} else if time.Until(dl) <= 0 {
			t.Error("deadline already passed")
		}
		_ = dl
	})
	for range 2 {
		select {
		case d := <-cut:
			if d < 30*time.Millisecond || d > time.Second {
				t.Fatalf("hung poll cut after %v, want about 40ms", d)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("a hung poll was never cut off")
		}
	}
	waitFor(t, 2*time.Second, "a poll after the hung ones", func() bool { return calls.Load() >= 3 })
}
