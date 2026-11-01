package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MelloB1989/karmax/fleet/internal/daemon"
)

// roster is a fake fleetd's /v1/status: each call serves the next of its
// rosters, the last one from then on; a nil roster answers 503.
type roster struct {
	mu    sync.Mutex
	calls int
	steps [][]daemon.Row
}

func (r *roster) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	if req.URL.Path != "/v1/status" {
		http.NotFound(w, req)
		return
	}
	r.mu.Lock()
	step := r.steps[min(r.calls, len(r.steps)-1)]
	r.calls++
	r.mu.Unlock()
	if step == nil {
		http.Error(w, "restarting", http.StatusServiceUnavailable)
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"agents": step})
}

func slots(agent, account string, mod func(*daemon.Row)) []daemon.Row {
	var rows []daemon.Row
	for i, name := range []string{agent, agent + ":2"} {
		r := daemon.Row{Agent: name, SlotOf: agent, Slot: i + 1, Account: account, State: "standby"}
		if mod != nil {
			mod(&r)
		}
		rows = append(rows, r)
	}
	return rows
}

func low(until time.Time) func(*daemon.Row) {
	return func(r *daemon.Row) { r.Low, r.FiveHour, r.FiveResets = true, 85, until }
}

func fleetOf(groups ...[]daemon.Row) []daemon.Row {
	var rows []daemon.Row
	for _, g := range groups {
		rows = append(rows, g...)
	}
	return rows
}

func quotaWaitOn(t *testing.T, r *roster, names []string, all bool, timeout time.Duration) (quotaWaitResult, string, int, error) {
	t.Helper()
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	var errOut bytes.Buffer
	sleeps := 0
	res, err := waitQuota(context.Background(), &client{base: srv.URL, token: "t"}, names, all, timeout, time.Millisecond, &errOut,
		func(ctx context.Context, d time.Duration) error { sleeps++; return sleepCtx(ctx, d) })
	return res, errOut.String(), sleeps, err
}

func TestQuotaWaitReturnsWhenAHeldAgentIsFree(t *testing.T) {
	until := time.Now().Add(2 * time.Hour)
	held := fleetOf(slots("agent-01", "acme", low(until)), slots("agent-02", "beta", nil))
	free := fleetOf(slots("agent-01", "acme", nil), slots("agent-02", "beta", nil))
	res, errOut, _, err := quotaWaitOn(t, &roster{steps: [][]daemon.Row{held, held, free}}, nil, false, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if res.Reason != "free" || strings.Join(res.Free, ",") != "agent-01" || len(res.Held) != 0 {
		t.Fatalf("result %+v, want agent-01 free", res)
	}
	if !strings.Contains(errOut, "  agent-01  5h 85%, lifts at ") || strings.Contains(errOut, "agent-02") {
		t.Errorf("stderr %q, want the one held agent and when it lifts", errOut)
	}
}

func TestQuotaWaitWithNothingHeldReturnsAtOnce(t *testing.T) {
	free := fleetOf(slots("agent-01", "acme", nil))
	res, _, sleeps, err := quotaWaitOn(t, &roster{steps: [][]daemon.Row{free}}, nil, false, time.Hour)
	if err != nil || res.Reason != "none" || sleeps != 0 {
		t.Fatalf("result %+v err %v after %d sleeps, want none at once", res, err, sleeps)
	}
	var out bytes.Buffer
	printQuotaWait(&out, res, nil, time.Hour, time.Now())
	if out.String() != "no agent is held back by its quota\n" {
		t.Errorf("printed %q", out.String())
	}
}

// A name is an agent, one of its slots, or an account; anything else is an
// error, not a wait for nothing.
func TestQuotaWaitTakesAnAgentASlotOrAnAccount(t *testing.T) {
	until := time.Now().Add(time.Hour)
	held := fleetOf(slots("agent-01", "acme", low(until)), slots("agent-02", "beta", low(until)))
	oneFree := fleetOf(slots("agent-01", "acme", low(until)), slots("agent-02", "beta", nil))
	for _, name := range []string{"agent-02", "agent-02:2", "beta"} {
		res, errOut, _, err := quotaWaitOn(t, &roster{steps: [][]daemon.Row{held, oneFree}}, []string{name}, false, time.Hour)
		if err != nil || res.Reason != "free" || strings.Join(res.Free, ",") != "agent-02" || strings.Contains(errOut, "agent-01") {
			t.Errorf("%s: result %+v err %v stderr %q, want agent-02 alone", name, res, err, errOut)
		}
	}
	if _, _, _, err := quotaWaitOn(t, &roster{steps: [][]daemon.Row{held}}, []string{"agent-9"}, false, time.Hour); err == nil ||
		!strings.Contains(err.Error(), "no agent or account agent-9") {
		t.Errorf("err %v, want the unknown name named", err)
	}
}

func TestQuotaWaitAllWaitsForEveryHeldAgent(t *testing.T) {
	until := time.Now().Add(time.Hour)
	both := fleetOf(slots("agent-01", "acme", low(until)), slots("agent-02", "beta", low(until)))
	one := fleetOf(slots("agent-01", "acme", nil), slots("agent-02", "beta", low(until)))
	none := fleetOf(slots("agent-01", "acme", nil), slots("agent-02", "beta", nil))
	r := &roster{steps: [][]daemon.Row{both, one, one, none}}
	res, _, _, err := quotaWaitOn(t, r, nil, true, time.Hour)
	if err != nil || res.Reason != "free" || strings.Join(res.Free, ",") != "agent-01,agent-02" || r.calls != 4 {
		t.Fatalf("result %+v err %v after %d reads, want both free after the fourth", res, err, r.calls)
	}
}

// One slot rate-limited holds its agent, and the agent is free only when
// every slot is.
func TestQuotaWaitHoldsAnAgentWhileAnyOfItsSlotsIsHeld(t *testing.T) {
	cooling := func(r *daemon.Row) {
		if r.Slot == 2 {
			r.State, r.Cooling = "cooling", time.Now().Add(time.Hour)
		}
	}
	held := fleetOf(slots("agent-01", "acme", cooling))
	free := fleetOf(slots("agent-01", "acme", nil))
	res, errOut, _, err := quotaWaitOn(t, &roster{steps: [][]daemon.Row{held, held, free}}, nil, false, time.Hour)
	if err != nil || res.Reason != "free" || !strings.Contains(errOut, "  agent-01  rate-limited, lifts at ") {
		t.Fatalf("result %+v err %v stderr %q", res, err, errOut)
	}
}

func TestQuotaWaitRidesOutAFleetdRestart(t *testing.T) {
	until := time.Now().Add(time.Hour)
	held := fleetOf(slots("agent-01", "acme", low(until)))
	free := fleetOf(slots("agent-01", "acme", nil))
	res, errOut, _, err := quotaWaitOn(t, &roster{steps: [][]daemon.Row{held, nil, nil, free}}, nil, false, time.Hour)
	if err != nil || res.Reason != "free" {
		t.Fatalf("result %+v err %v", res, err)
	}
	if strings.Count(errOut, "fleetd unreachable, retrying") != 1 {
		t.Errorf("stderr %q, want the restart said once", errOut)
	}
}

func TestQuotaWaitTimesOutWithWhatStillHolds(t *testing.T) {
	held := fleetOf(slots("agent-01", "acme", low(time.Time{})))
	res, _, _, err := quotaWaitOn(t, &roster{steps: [][]daemon.Row{held}}, nil, false, 30*time.Millisecond)
	if err != nil || res.Reason != "timeout" || len(res.Held) != 1 || res.Held[0].Agent != "agent-01" {
		t.Fatalf("result %+v err %v, want a timeout with agent-01 held", res, err)
	}
	var out bytes.Buffer
	printQuotaWait(&out, res, nil, 30*time.Millisecond, time.Now())
	if !strings.Contains(out.String(), "still held after 30ms:\n  agent-01  5h 85%, lifts when a turn reports its new window") {
		t.Errorf("printed %q", out.String())
	}
}
