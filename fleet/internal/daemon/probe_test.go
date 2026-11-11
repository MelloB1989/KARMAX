package daemon

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/MelloB1989/karmax/fleet/internal/quotaprobe"
	"github.com/MelloB1989/karmax/fleet/internal/reconcile"
)

type fakeReader map[string]quotaprobe.Reading

func (r fakeReader) Read(_ context.Context, tok string) (quotaprobe.Reading, error) {
	if v, ok := r[tok]; ok {
		return v, nil
	}
	return quotaprobe.Reading{}, errors.New("no rate limits in the answer (HTTP 401)")
}

func TestTheLiveReadingBeatsAnIdleStatusLine(t *testing.T) {
	f, _ := newPool(t)
	acct := func(n string) string { w, _ := f.cfg.Worker(n); return w.Account }
	resets := t0.Add(2 * time.Hour)
	week := t0.Add(48 * time.Hour)
	// agent-01's status line is from a turn long ago: 10 % used. The account
	// has since spent 55 % of its window and 70 % of its week.
	set(f, "agent-01", func(s *reconcile.State) {
		s.Quota = reconcile.Quota{FiveHour: 10, FiveHourResets: resets, SevenDay: 20, SevenDayResets: week, At: t0.Add(-5 * time.Hour)}
	})
	f.ProbeQuota(fakeReader{"tok-1": {FiveHour: 55, FiveResets: resets, SevenDay: 70, SevenResets: week}},
		map[string]string{acct("agent-01"): "tok-1", acct("agent-02"): "tok-bad"}, time.Minute)
	f.probeAll(context.Background(), t0)

	st := f.sharedQuota(f.State("agent-01"), t0)
	if st.Quota.FiveHour != 55 || st.Quota.SevenDay != 70 {
		t.Errorf("worker sees %+v, want the live 55/70", st.Quota)
	}
	qs, err := f.db.LatestQuota()
	if err != nil {
		t.Fatal(err)
	}
	if q := qs[acct("agent-01")]; q.FiveHour != 55 || q.SevenDay != 70 || q.Source != "probe" {
		t.Errorf("recorded %+v", q)
	}
	if _, ok := f.Probed(acct("agent-02"), t0, time.Hour); ok {
		t.Error("an account whose read failed has a live reading")
	}
	if _, ok := f.Probed(acct("agent-01"), t0.Add(30*time.Minute), 15*time.Minute); ok {
		t.Error("a reading half an hour old is live")
	}
}

func TestTheProbeRunsOnTheTickOnItsInterval(t *testing.T) {
	f, _ := newPool(t)
	w, _ := f.cfg.Worker("agent-01")
	f.ProbeQuota(fakeReader{"tok-1": {FiveHour: 5, SevenDay: 6}}, map[string]string{w.Account: "tok-1"}, time.Minute)
	f.probeDue(t0)
	f.probeWait()
	if _, ok := f.Probed(w.Account, t0, time.Minute); !ok {
		t.Fatal("not read on the first tick")
	}
	f.qprobe.mu.Lock()
	delete(f.qprobe.got, w.Account)
	f.qprobe.mu.Unlock()
	f.probeDue(t0.Add(30 * time.Second))
	f.probeWait()
	if _, ok := f.Probed(w.Account, t0.Add(30*time.Second), time.Minute); ok {
		t.Error("read again within its interval")
	}
	f.probeDue(t0.Add(61 * time.Second))
	f.probeWait()
	if _, ok := f.Probed(w.Account, t0.Add(61*time.Second), time.Minute); !ok {
		t.Error("not read after its interval")
	}
}

func (f *Fleet) probeWait() { f.qprobe.wg.Wait() }
