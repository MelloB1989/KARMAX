package daemon

import (
	"math"
	"strings"
	"testing"
	"time"

	"github.com/MelloB1989/karmax/fleet/internal/ledger"
	"github.com/MelloB1989/karmax/fleet/internal/reconcile"
)

func TestWeekRoomIsTheWeekLeftADay(t *testing.T) {
	for _, c := range []struct {
		name   string
		seven  float64
		resets time.Time
		want   float64
	}{
		{"no reset known: a week away", 0, time.Time{}, 100.0 / 7},
		{"half left over two days", 50, t0.Add(48 * time.Hour), 25},
		{"spent", 100, t0.Add(24 * time.Hour), 0},
		{"over, never below nothing", 104, t0.Add(24 * time.Hour), 0},
		{"minutes from its reset: an hour", 40, t0.Add(10 * time.Minute), 60 * 24},
	} {
		if got := WeekRoom(c.seven, c.resets, t0); math.Abs(got-c.want) > 1e-9 {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
	if weekTier(14) != weekTier(16) || weekTier(14) == weekTier(7) || weekTier(0) != weekTier(0.01) {
		t.Errorf("tiers: 14→%d 16→%d 7→%d 0→%d", weekTier(14), weekTier(16), weekTier(7), weekTier(0))
	}
}

func TestPickWeighsTheWeekFirst(t *testing.T) {
	f, _ := newPool(t)
	week := func(name string, five, seven float64, resets time.Duration) {
		set(f, name, func(s *reconcile.State) {
			s.Quota.FiveHour, s.Quota.SevenDay, s.Quota.SevenDayResets = five, seven, t0.Add(resets)
		})
	}
	week("agent-01", 0, 80, 96*time.Hour) // a fresh 5 h, but a week nearly spent four days early
	week("agent-02", 30, 20, 120*time.Hour)
	week("agent-03", 10, 60, 3*time.Hour) // 40 % left that is lost in three hours
	r := f.Pick("", t0)
	if r.Agent != "agent-03" || !strings.Contains(r.Why, "%/day") {
		t.Fatalf("the week about to reset with room left goes first: %+v", r)
	}
	if got := []string{r.Candidates[1].Agent, r.Candidates[2].Agent}; got[0] != "agent-02" || got[1] != "agent-01" {
		t.Errorf("then the week on pace, the 5-hour window notwithstanding: %v", got)
	}
	if r.Candidates[2].WeekRoom != 5 {
		t.Errorf("agent-01's room: %v, want 20 %% over 4 days", r.Candidates[2].WeekRoom)
	}

	// Its last task in the repo does not send work to a week nearly spent...
	set(f, "agent-03", func(s *reconcile.State) { s.Phase = reconcile.Disabled })
	if err := f.db.OpenTask("old", "agent-01", "operator", t0.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := f.db.SetTaskRepo("old", "/code/site"); err != nil {
		t.Fatal(err)
	}
	if err := f.db.CloseTask("old", t0, "done", "", ""); err != nil {
		t.Fatal(err)
	}
	if r := f.Pick("/code/site", t0); r.Agent != "agent-02" {
		t.Errorf("affinity over a spent week: %+v", r)
	}
	// ...but between weeks as roomy, it decides.
	week("agent-01", 0, 22, 120*time.Hour)
	if r := f.Pick("/code/site", t0); r.Agent != "agent-01" || !strings.Contains(r.Why, "/code/site") {
		t.Errorf("weeks level, the repo decides: %+v", r)
	}
}

func TestAccountsCarryTheirWeekRoom(t *testing.T) {
	f, _ := newPool(t)
	now := time.Now()
	if err := f.db.AddQuota(ledger.Quota{Account: "agent-02", At: now, SevenDay: 30, SevenResets: now.Add(70 * time.Hour), Source: "statusline"}); err != nil {
		t.Fatal(err)
	}
	for _, a := range f.accounts(now) {
		if a.Name == "agent-02" {
			if want := WeekRoom(30, now.Add(70*time.Hour).Truncate(time.Second), now); math.Abs(a.WeekRoom-want) > 0.01 {
				t.Errorf("week room %v, want %v", a.WeekRoom, want)
			}
			return
		}
	}
	t.Fatal("no account agent-02")
}
