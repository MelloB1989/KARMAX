package daemon

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/MelloB1989/karmax/fleet/internal/observe"
	"github.com/MelloB1989/karmax/fleet/internal/reconcile"
)

func quotaPushes(out *outbox) []string {
	out.mu.Lock()
	defer out.mu.Unlock()
	var ps []string
	for _, p := range out.pushes {
		if strings.HasPrefix(p, "fleet.agent.quota_") || strings.HasPrefix(p, "fleet.agent.cooling") {
			ps = append(ps, p)
		}
	}
	return ps
}

func windows(t *testing.T, session string, five float64, fiveResets time.Time, seven float64, sevenResets time.Time) *observe.Status {
	t.Helper()
	s, err := observe.ParseStatus([]byte(`{"session_id":"` + session + `","rate_limits":{"five_hour":{"used_percentage":` + itoa(int(five)) +
		`,"resets_at":` + itoa(int(fiveResets.Unix())) + `},"seven_day":{"used_percentage":` + itoa(int(seven)) +
		`,"resets_at":` + itoa(int(sevenResets.Unix())) + `}}}`))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// An agent whose 5-hour window passes low_5h is held back from new tasks:
// the orchestrator hears it once, from slot 1 for both slots, with when it
// lifts; the roster's rows say the same. When the window resets it hears,
// once, that the agent is free again.
func TestTheOrchestratorHearsWhenAQuotaHoldBeginsAndWhenItLifts(t *testing.T) {
	f, boxes, out := newSlots(t)
	resets := t0.Add(time.Hour)
	for _, n := range []string{"agent-01", "agent-01:2"} {
		boxes[n].obs.Status = windows(t, "s-"+n, 85, resets, 40, t0.Add(48*time.Hour))
	}
	for i := range 3 {
		f.Tick(context.Background(), t0.Add(time.Duration(i)*30*time.Second))
	}
	if got := quotaPushes(out); len(got) != 1 || got[0] != "fleet.agent.quota_held agent-01" {
		t.Fatalf("pushes %q, want one quota_held for agent-01", got)
	}
	for _, r := range f.Status(t0.Add(time.Minute)) {
		h := r.Hold()
		if r.SlotOf == "agent-01" && (!h.Held() || !h.Until.Equal(resets) || h.Why[0] != "5h 85%") {
			t.Errorf("%s: hold %+v, want 5h 85%% until %v", r.Agent, h, resets)
		}
		if r.SlotOf == "agent-02" && h.Held() {
			t.Errorf("%s: hold %+v, want none", r.Agent, h)
		}
	}

	for _, n := range []string{"agent-01", "agent-01:2"} {
		boxes[n].obs.Status, _ = observe.ParseStatus([]byte(`{"session_id":"s-` + n + `"}`))
	}
	for i := range 3 {
		f.Tick(context.Background(), resets.Add(time.Duration(i)*30*time.Second))
	}
	want := []string{"fleet.agent.quota_held agent-01", "fleet.agent.quota_reset agent-01"}
	if got := quotaPushes(out); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("pushes %q, want %q", got, want)
	}
	if st := f.State("agent-01"); st.QuotaHeld {
		t.Error("agent-01 still marked held after it was told free")
	}
}

// A rate limit alone is told as cooling, with its end; its lifting is told
// as quota_reset, as a quota hold's is.
func TestARateLimitIsToldOnceAndItsEndAsAReset(t *testing.T) {
	f, boxes, out := newSlots(t)
	resets := t0.Add(time.Hour)
	boxes["agent-01"].obs.Status = windows(t, "s-agent-01", 40, resets, 20, t0.Add(48*time.Hour))
	f.Tick(context.Background(), t0)
	boxes["agent-01"].obs.Events = []observe.Event{{T: t0.Unix(), Event: "StopFailure", Session: "s-agent-01",
		Raw: []byte(`{"hook_event_name":"StopFailure","error":{"type":"rate_limit"}}`)}}
	f.Tick(context.Background(), t0.Add(time.Minute))
	boxes["agent-01"].obs.Events = nil
	f.Tick(context.Background(), t0.Add(2*time.Minute))
	if st := f.State("agent-01"); st.Phase != reconcile.Cooling || !st.QuotaHeld {
		t.Fatalf("agent-01 %s held %v, want cooling and held", st.Phase, st.QuotaHeld)
	}
	if got := quotaPushes(out); len(got) != 1 || got[0] != "fleet.agent.cooling agent-01" {
		t.Fatalf("pushes %q, want the cooling push alone", got)
	}
	boxes["agent-01"].obs.Status, _ = observe.ParseStatus([]byte(`{"session_id":"s-agent-01"}`))
	f.Tick(context.Background(), resets.Add(time.Second))
	f.Tick(context.Background(), resets.Add(time.Minute))
	want := []string{"fleet.agent.cooling agent-01", "fleet.agent.quota_reset agent-01"}
	if got := quotaPushes(out); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("pushes %q, want %q", got, want)
	}
}

// A hold's end that comes while the agent is down is told when it is back.
func TestAHoldThatLiftsWhileTheAgentIsDownIsToldWhenItIsBack(t *testing.T) {
	f, boxes, out := newSlots(t)
	resets := t0.Add(time.Hour)
	boxes["agent-01"].obs.Status = windows(t, "s-agent-01", 95, resets, 20, t0.Add(48*time.Hour))
	f.Tick(context.Background(), t0)
	boxes["agent-01"].obs.Status, _ = observe.ParseStatus([]byte(`{"session_id":"s-agent-01"}`))
	boxes["agent-01"].obs.ContainerUp = false
	f.Tick(context.Background(), resets.Add(time.Second))
	if got := quotaPushes(out); len(got) != 1 {
		t.Fatalf("pushes %q while down, want only the hold", got)
	}
	boxes["agent-01"].obs = healthy("agent-01")
	f.Tick(context.Background(), resets.Add(time.Minute))
	f.Tick(context.Background(), resets.Add(2*time.Minute))
	if got := quotaPushes(out); len(got) != 2 || got[1] != "fleet.agent.quota_reset agent-01" {
		t.Fatalf("pushes %q, want the reset told once it is back", got)
	}
}

func TestAHoldLiftsWhenTheLastOfItsReasonsDoes(t *testing.T) {
	five, seven, cool := t0.Add(time.Hour), t0.Add(72*time.Hour), t0.Add(30*time.Minute)
	h := hold(reconcile.Cooling, cool, true, true, 92, 95, five, seven)
	if strings.Join(h.Why, ",") != "rate-limited,5h 92%,7d 95%" || !h.Until.Equal(seven) {
		t.Errorf("hold %+v, want all three until the 7-day reset", h)
	}
	if h := hold(reconcile.Standby, time.Time{}, true, false, 92, 10, time.Time{}, seven); !h.Held() || !h.Until.IsZero() {
		t.Errorf("hold %+v, want held with its end not known", h)
	}
	if h := hold(reconcile.Working, time.Time{}, false, false, 50, 50, five, seven); h.Held() {
		t.Errorf("hold %+v, want none", h)
	}
	if got := When(t0.Add(2*time.Hour+13*time.Minute), t0); !strings.HasSuffix(got, "(in 2h13m)") {
		t.Errorf("When = %q", got)
	}
	held := reconcile.State{Phase: reconcile.Working, Task: "t1", Low: true, Quota: reconcile.Quota{FiveHour: 85, FiveHourResets: five}}
	next, acts := quotaNews("agent-01", reconcile.State{Phase: reconcile.Working, Task: "t1"}, held, t0)
	if len(acts) != 1 || !next.QuotaHeld || !strings.Contains(acts[0].Text, "agent-01 is held back by its quota (5h 85%)") ||
		!strings.Contains(acts[0].Text, "(in 1h)") {
		t.Errorf("held: %+v %+v", next.QuotaHeld, acts)
	}
	free := reconcile.State{Phase: reconcile.Standby, QuotaHeld: true}
	if _, acts := quotaNews("agent-01", next, free, five); len(acts) != 1 || acts[0].Text != "agent-01 is free of its quota hold (it was 5h 85%): it takes tasks again" {
		t.Errorf("free: %+v", acts)
	}
}
