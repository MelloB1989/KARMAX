package aws

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/MelloB1989/karmax/internal/sources"
)

func costConfig(profiles ...string) Config {
	return Config{Profiles: profiles, SpikeFactor: 1.5, SpikeFloor: 5, TrailingDays: 7, CostRecheck: 6 * time.Hour}
}

// flat is a month of steady spend, so only what a test changes stands out.
func flat(amount float64) map[string]float64 {
	days := map[string]float64{}
	for d := time.Date(2026, 9, 20, 0, 0, 0, 0, time.UTC); d.Before(time.Date(2026, 11, 3, 0, 0, 0, 0, time.UTC)); d = d.AddDate(0, 0, 1) {
		days[d.Format(dateLayout)] = amount
	}
	return days
}

func ofType(evs []sources.Event, typ string) []sources.Event {
	var out []sources.Event
	for _, e := range evs {
		if e.Type == typ {
			out = append(out, e)
		}
	}
	return out
}

// Each Cost Explorer call costs a cent: one full read a day, then at most a
// one-call look at yesterday and today every few hours, and a restart does
// not start the day over.
func TestCostIsReadInFullOnceADayThenRecheckedCheaply(t *testing.T) {
	acct := &fakeAPI{account: "111122223333", days: flat(10)}
	h := newHarness(t, costConfig("ops-admin"), map[string]*fakeAPI{"ops-admin": acct})
	poll := func() {
		t.Helper()
		if _, err := h.src.PollCost(context.Background()); err != nil {
			t.Fatal(err)
		}
	}

	poll() // 01:00
	if acct.count("DailyCost") != 1 || acct.count("Anomalies") != 1 || acct.count("Budgets") != 1 {
		t.Fatalf("full read calls = %v", acct.calls)
	}
	if got := acct.costAsk[0]; got != [2]string{"2026-10-01", "2026-11-01"} {
		t.Errorf("full read asked for %v; want the month and the trailing week through today", got)
	}

	h.now = h.now.Add(time.Hour)
	poll()
	if acct.count("DailyCost") != 1 {
		t.Fatalf("an hourly poll inside the recheck interval called Cost Explorer: %v", acct.calls)
	}

	// A restart keeps the day's cache.
	restarted := newHarness(t, costConfig("ops-admin"), map[string]*fakeAPI{"ops-admin": acct})
	restarted.kv, restarted.now = h.kv, h.now.Add(time.Hour)
	if _, err := restarted.src.PollCost(context.Background()); err != nil {
		t.Fatal(err)
	}
	if acct.count("DailyCost") != 1 {
		t.Fatalf("a restart read the day again: %v", acct.calls)
	}

	h.now = time.Date(2026, 10, 31, 7, 0, 0, 0, time.UTC)
	poll()
	if acct.count("DailyCost") != 2 || acct.count("Anomalies") != 1 {
		t.Fatalf("recheck calls = %v", acct.calls)
	}
	if got := acct.costAsk[1]; got != [2]string{"2026-10-30", "2026-11-01"} {
		t.Errorf("recheck asked for %v; want yesterday and today only", got)
	}

	h.now = time.Date(2026, 11, 1, 0, 30, 0, 0, time.UTC)
	poll()
	if acct.count("DailyCost") != 3 || acct.count("Anomalies") != 2 {
		t.Fatalf("a new day did not read in full: %v", acct.calls)
	}
}

// Memory keeps the trend: one quiet summary a day per account.
func TestYesterdaysSpendIsSummarisedOnceADay(t *testing.T) {
	acct := &fakeAPI{account: "111122223333", days: flat(10)}
	acct.days["2026-10-30"] = 12.34
	h := newHarness(t, costConfig("ops-admin"), map[string]*fakeAPI{"ops-admin": acct})
	h.src.PollCost(context.Background())
	daily := ofType(h.take(), "cost.daily")
	if len(daily) != 1 {
		t.Fatalf("daily summaries = %+v", daily)
	}
	e := daily[0]
	if e.Urgent || e.Account != "111122223333/global" || e.Source != "aws" {
		t.Errorf("summary = %s urgent=%v", e.Account, e.Urgent)
	}
	for _, want := range []string{"2026-10-30", "12.34", "month to date", "302.34"} {
		if !strings.Contains(strings.ToLower(e.Summary), strings.ToLower(want)) {
			t.Errorf("summary %q lacks %q", e.Summary, want)
		}
	}
	h.now = h.now.Add(7 * time.Hour)
	h.src.PollCost(context.Background())
	if again := ofType(h.take(), "cost.daily"); len(again) != 0 {
		t.Fatalf("the day was summarised twice: %+v", again)
	}
}

// An account that spends nothing does not fill memory with zeros.
func TestAnIdleAccountIsNotSummarised(t *testing.T) {
	acct := &fakeAPI{account: "111122223333", days: flat(0)}
	h := newHarness(t, costConfig("ops-admin"), map[string]*fakeAPI{"ops-admin": acct})
	h.src.PollCost(context.Background())
	if evs := h.take(); len(evs) != 0 {
		t.Fatalf("events for an idle account: %+v", evs)
	}
}

// A day is a spike when it beats the trailing mean by the factor AND clears
// the floor; either alone is not enough.
func TestTheSpikeRuleNeedsBothTheFactorAndTheFloor(t *testing.T) {
	for _, tc := range []struct {
		name          string
		trailing, day float64
		spike         bool
	}{
		{"three times a busy week", 10, 30, true},
		{"just under the factor", 10, 14.9, false},
		{"triple but pennies", 1, 3, false},
		{"new spend from nothing", 0, 6, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			acct := &fakeAPI{account: "111122223333", days: flat(tc.trailing)}
			acct.days["2026-10-30"] = tc.day
			h := newHarness(t, costConfig("ops-admin"), map[string]*fakeAPI{"ops-admin": acct})
			h.src.PollCost(context.Background())
			spikes := ofType(h.take(), "cost.anomaly")
			if got := len(spikes) == 1; got != tc.spike {
				t.Fatalf("spike = %v, want %v: %+v", got, tc.spike, spikes)
			}
			if tc.spike {
				e := spikes[0]
				if !e.Urgent || e.Data["kind"] != "spike" || e.Data["date"] != "2026-10-30" {
					t.Errorf("spike event = %+v", e)
				}
				if !strings.Contains(e.Summary, "1.5") {
					t.Errorf("summary %q does not state the rule", e.Summary)
				}
			}
			h.now = h.now.Add(7 * time.Hour)
			h.src.PollCost(context.Background())
			if again := ofType(h.take(), "cost.anomaly"); len(again) != 0 {
				t.Fatalf("the spike was told twice: %+v", again)
			}
		})
	}
}

// A runaway day shows before it is over: today so far already past the rule
// is a spike at the next recheck.
func TestTodaySoFarPastTheRuleIsCaughtAtARecheck(t *testing.T) {
	acct := &fakeAPI{account: "111122223333", days: flat(10)}
	acct.days["2026-10-31"] = 1
	h := newHarness(t, costConfig("ops-admin"), map[string]*fakeAPI{"ops-admin": acct})
	h.src.PollCost(context.Background())
	h.take()

	acct.days["2026-10-31"] = 40
	h.now = h.now.Add(6 * time.Hour)
	h.src.PollCost(context.Background())
	spikes := ofType(h.take(), "cost.anomaly")
	if len(spikes) != 1 || spikes[0].Data["date"] != "2026-10-31" || !spikes[0].Urgent {
		t.Fatalf("spikes = %+v", spikes)
	}
	if !strings.Contains(strings.ToLower(spikes[0].Title), "so far") {
		t.Errorf("title %q does not say the day is not over", spikes[0].Title)
	}
}

// A budget AWS reports as crossed is urgent, once per budget period.
func TestACrossedBudgetIsUrgentOncePerPeriod(t *testing.T) {
	acct := &fakeAPI{account: "111122223333", days: flat(10)}
	acct.budgets = []Budget{{
		Name: "example-monthly", TimeUnit: "MONTHLY", Limit: 300, Actual: 290, Forecast: 330, Unit: "USD",
		Notifications: []BudgetNotification{
			{Type: "ACTUAL", Comparison: "GREATER_THAN", Threshold: 80, ThresholdType: "PERCENTAGE", InAlarm: true},
			{Type: "FORECASTED", Comparison: "GREATER_THAN", Threshold: 120, ThresholdType: "PERCENTAGE", InAlarm: false},
		},
	}}
	h := newHarness(t, costConfig("ops-admin"), map[string]*fakeAPI{"ops-admin": acct})
	h.src.PollCost(context.Background())
	evs := ofType(h.take(), "cost.anomaly")
	if len(evs) != 1 || !evs[0].Urgent || evs[0].Data["kind"] != "budget" {
		t.Fatalf("budget events = %+v", evs)
	}
	// Crossed before KARMAX ever read this account: context, not a page.
	if !evs[0].Backlog {
		t.Errorf("the first read's budget was published as news: %+v", evs[0])
	}
	for _, want := range []string{"example-monthly", "80%", "290", "300"} {
		if !strings.Contains(evs[0].Summary, want) {
			t.Errorf("summary %q lacks %q", evs[0].Summary, want)
		}
	}
	h.now = h.now.Add(7 * time.Hour)
	h.src.PollCost(context.Background())
	if again := ofType(h.take(), "cost.anomaly"); len(again) != 0 {
		t.Fatalf("the budget was told twice in one month: %+v", again)
	}
	h.now = time.Date(2026, 11, 2, 1, 0, 0, 0, time.UTC)
	h.src.PollCost(context.Background())
	if next := ofType(h.take(), "cost.anomaly"); len(next) != 1 || next[0].Backlog {
		t.Fatalf("a new month's crossing was not told as news: %+v", next)
	}
}

// What Cost Anomaly Detection finds is told once; small ones are not urgent,
// and ones the operator dismissed are not told.
func TestAnomalyDetectionFindingsAreToldOnce(t *testing.T) {
	acct := &fakeAPI{account: "111122223333", days: flat(10)}
	acct.anomalies = []Anomaly{
		{ID: "a-big", Start: "2026-10-30", Dimension: "Amazon Elastic Compute Cloud - Compute", Impact: 20, Actual: 30, Expected: 10, RootCauses: []string{"AmazonEC2 us-east-1"}},
		{ID: "a-small", Start: "2026-10-29", Dimension: "Amazon Simple Storage Service", Impact: 1, Actual: 2, Expected: 1},
		{ID: "a-dismissed", Start: "2026-10-28", Dimension: "AWS Lambda", Impact: 50, Dismissed: true},
	}
	h := newHarness(t, costConfig("ops-admin"), map[string]*fakeAPI{"ops-admin": acct})
	h.src.PollCost(context.Background())
	evs := ofType(h.take(), "cost.anomaly")
	got := map[string]bool{}
	for _, e := range evs {
		if e.Data["kind"] != "detector" {
			t.Fatalf("unexpected anomaly event %+v", e)
		}
		got[e.Data["id"].(string)] = e.Urgent
	}
	if len(got) != 2 || !got["a-big"] || got["a-small"] {
		t.Fatalf("findings told (id -> urgent) = %v", got)
	}
	h.now = time.Date(2026, 11, 1, 1, 0, 0, 0, time.UTC)
	h.src.PollCost(context.Background())
	if again := ofType(h.take(), "cost.anomaly"); len(again) != 0 {
		t.Fatalf("findings were told again: %+v", again)
	}
}

// An account that may not use Cost Explorer is asked again tomorrow, not
// every hour, and health says why its cost is missing.
func TestARefusedCostExplorerIsAskedAgainTomorrow(t *testing.T) {
	acct := &fakeAPI{account: "111122223333", costErr: &DeniedError{Err: errors.New("ce:GetCostAndUsage")},
		anomErr: &DeniedError{Err: errors.New("ce:GetAnomalies")}, budgetErr: &DeniedError{Err: errors.New("budgets:ViewBudget")}}
	h := newHarness(t, costConfig("ops-admin"), map[string]*fakeAPI{"ops-admin": acct})
	if _, err := h.src.PollCost(context.Background()); err != nil {
		t.Fatalf("a refusal failed the poll: %v", err)
	}
	for _, at := range []time.Duration{time.Hour, 7 * time.Hour, 13 * time.Hour} {
		h.now = time.Date(2026, 10, 31, 1, 0, 0, 0, time.UTC).Add(at)
		h.src.PollCost(context.Background())
	}
	if acct.count("DailyCost") != 1 {
		t.Fatalf("Cost Explorer asked %d times in a day after refusing", acct.count("DailyCost"))
	}
	if hl := h.health("ops-admin"); !strings.Contains(hl.Detail, "Cost Explorer") || hl.Status != StatusDegraded {
		t.Errorf("health = %+v", hl)
	}
	h.now = time.Date(2026, 11, 1, 1, 0, 0, 0, time.UTC)
	h.src.PollCost(context.Background())
	if acct.count("DailyCost") != 2 {
		t.Fatalf("not asked again the next day: %d", acct.count("DailyCost"))
	}
}

// A passing failure (throttling, the network) is tried again next poll.
func TestAPassingCostFailureIsRetriedNextPoll(t *testing.T) {
	acct := &fakeAPI{account: "111122223333", days: flat(10), costErr: errors.New("ThrottlingException")}
	h := newHarness(t, costConfig("ops-admin"), map[string]*fakeAPI{"ops-admin": acct})
	if _, err := h.src.PollCost(context.Background()); err == nil {
		t.Fatal("a poll that read no cost at all reported success")
	}
	acct.costErr = nil
	h.now = h.now.Add(time.Hour)
	if _, err := h.src.PollCost(context.Background()); err != nil {
		t.Fatal(err)
	}
	if acct.count("DailyCost") != 2 || len(ofType(h.take(), "cost.daily")) != 1 {
		t.Fatalf("calls %v", acct.calls)
	}
}

// An outage that keeps every account from being reached fails the poll, so
// loop health shows it; it is not a quiet success.
func TestUnreachableAccountsFailTheCostPoll(t *testing.T) {
	down := &fakeAPI{account: "111122223333", identityErr: errors.New("dial tcp: no route to host")}
	h := newHarness(t, costConfig("ops-admin"), map[string]*fakeAPI{"ops-admin": down})
	if _, err := h.src.PollCost(context.Background()); err == nil {
		t.Fatal("a cost poll that reached no account reported success")
	}
}

// Cost is per account: profiles on one account in different regions are one
// bill, read once.
func TestCostIsReadOncePerAccount(t *testing.T) {
	admin := &fakeAPI{account: "111122223333", days: flat(10)}
	eu := &fakeAPI{account: "111122223333", days: flat(10)}
	h := newHarness(t, costConfig("ops-admin", "ops-eu"), map[string]*fakeAPI{"ops-admin": admin, "ops-eu": eu})
	rep, err := h.src.PollCost(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if admin.count("DailyCost")+eu.count("DailyCost") != 1 {
		t.Fatalf("one account's cost read %d times", admin.count("DailyCost")+eu.count("DailyCost"))
	}
	if rep.BilledCalls != 2 {
		t.Errorf("billed calls = %d, want 2 (daily cost and anomalies)", rep.BilledCalls)
	}
	for _, d := range h.dialed {
		if !strings.HasSuffix(d, "@us-east-1") {
			t.Errorf("cost read through %s; billing answers in us-east-1", d)
		}
	}
}
