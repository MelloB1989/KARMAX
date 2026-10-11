package console

import (
	"context"
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
	if a2.Live || a2.CurrentTool != nil || a2.Container != nil || a2.Term != (TermSize{}) || a2.Model != "" ||
		a2.ContextPct != 0 || !a2.StatuslineAt.IsZero() || !a2.LastEventAt.IsZero() {
		t.Fatalf("remote agent has live fields: %+v", a2)
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
