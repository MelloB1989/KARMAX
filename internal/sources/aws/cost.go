package aws

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/MelloB1989/karmax/internal/sources"
)

const costGroup = "aws-cost"

// costKeepDays bounds the per-account day cache: a month and the trailing
// window, with room to spare.
const costKeepDays = 45

// anomalyLookbackDays is how far back each daily read asks Cost Anomaly
// Detection; findings already told are deduplicated by id.
const anomalyLookbackDays = 7

// CostReport is what one cost poll did.
type CostReport struct {
	Readers     int // accounts read
	BilledCalls int // Cost Explorer calls made, at a cent each
	Published   int
	Fixes       []string
}

// costState is one account's cost cache, kept in the store so a restart
// does not spend the day's calls again.
type costState struct {
	Full    string             `json:"full"`    // UTC date of the last full read
	Checked time.Time          `json:"checked"` // last Cost Explorer read of the day's cost
	Days    map[string]float64 `json:"days"`
	Unit    string             `json:"unit,omitempty"`
	Refused map[string]string  `json:"refused,omitempty"` // what refused -> the UTC date it did
}

// What may refuse, in the words health uses.
const (
	refusedCost      = "Cost Explorer"
	refusedAnomalies = "Cost Anomaly Detection"
	refusedBudgets   = "Budgets"
)

// PollCost reads each account's spend: in full on the first poll of a UTC
// day (daily cost for the month and the trailing window, anomaly findings,
// budgets), then at most one recheck of yesterday and today per CostRecheck.
// Every other poll makes no billed call.
func (s *Source) PollCost(ctx context.Context) (CostReport, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var rep CostReport
	readers, health, err := s.plan(ctx, false)
	if err != nil {
		return rep, err
	}
	now := s.now()
	ok, broken := 0, 0
	var lastErr error
	for _, r := range readers {
		res, err := s.costOf(ctx, r, now)
		rep.BilledCalls += res.billed
		rep.Published += res.published
		if err != nil {
			h := health[r.profile.Name]
			if isAuth(err) {
				h = failure(r.profile, err)
				h.Account = r.label
			} else {
				h.Status, h.Detail = StatusDegraded, "cost: "+clip(err.Error(), 240)
				broken++
				lastErr = err
			}
			health[r.profile.Name] = h
			s.health[r.profile.Name] = h
			continue
		}
		ok++
		rep.Readers++
		s.costNotes[r.profile.Name] = res.note
	}
	// Alarm polls own health; a profile they have not seen yet takes this one's word.
	for name, h := range health {
		if _, seen := s.health[name]; !seen {
			h.CheckedAt = now
			s.health[name] = h
		}
	}
	rep.Fixes = fixes(health)
	if ok == 0 && broken > 0 {
		return rep, fmt.Errorf("no AWS account's cost could be read: %w", lastErr)
	}
	return rep, nil
}

type costResult struct {
	billed, published int
	note              string
}

func (s *Source) costOf(ctx context.Context, r reader, now time.Time) (costResult, error) {
	var res costResult
	st, err := s.loadCost(r.account)
	if err != nil {
		return res, err
	}
	utc := now.UTC()
	today := utc.Format(dateLayout)
	day := func(offset int) string { return utc.AddDate(0, 0, offset).Format(dateLayout) }
	refusedToday := func(what string) bool { return st.Refused[what] == today }
	refuse := func(what string) { st.Refused[what] = today }

	full := st.Full != today
	recheck := !full && s.cfg.CostRecheck > 0 && utc.Sub(st.Checked) >= s.cfg.CostRecheck
	if !full && !recheck {
		res.note = refusedNote(st, today)
		return res, nil
	}

	var anomalies []Anomaly
	if !refusedToday(refusedCost) {
		from := day(-(s.cfg.TrailingDays + 1))
		if !full {
			from = day(-1)
		} else if first := utc.AddDate(0, 0, -1).Format("2006-01") + "-01"; first < from {
			from = first // yesterday's month from its first day, for month to date
		}
		res.billed++
		days, err := r.api.DailyCost(ctx, from, day(1))
		switch {
		case err == nil:
			for _, d := range days {
				st.Days[d.Date] = d.Amount
				if d.Unit != "" {
					st.Unit = d.Unit
				}
			}
			st.Checked = utc
		case isDenied(err):
			refuse(refusedCost)
		default:
			return res, err // not cached: the next poll tries again
		}
	}
	if full {
		st.Full = today
		if !refusedToday(refusedAnomalies) {
			res.billed++
			found, err := r.api.Anomalies(ctx, day(-anomalyLookbackDays))
			switch {
			case err == nil:
				anomalies = found
			case isDenied(err):
				refuse(refusedAnomalies)
			default:
				res.note = joinDetail(res.note, "anomaly detection: "+clip(err.Error(), 120))
			}
		}
	}
	var budgets []Budget
	if !refusedToday(refusedBudgets) {
		found, err := r.api.Budgets(ctx, r.account)
		switch {
		case err == nil:
			budgets = found
		case isDenied(err):
			refuse(refusedBudgets)
		default:
			res.note = joinDetail(res.note, "budgets: "+clip(err.Error(), 120))
		}
	}
	st.prune(utc)
	if err := s.saveCost(r.account, st); err != nil {
		return res, err
	}
	res.note = joinDetail(refusedNote(st, today), res.note)

	var evs []sources.Event
	if full {
		if e, ok := s.dailySummary(r, st, utc); ok {
			evs = append(evs, e)
		}
	}
	for _, d := range []string{day(-1), today} {
		if e, ok := s.spike(r, st, d, d == today); ok {
			evs = append(evs, e)
		}
	}
	for _, a := range anomalies {
		if !a.Dismissed {
			evs = append(evs, s.anomalyEvent(r, a, utc))
		}
	}
	for _, b := range budgets {
		for _, n := range b.Notifications {
			if n.InAlarm {
				evs = append(evs, s.budgetEvent(r, b, n, utc))
			}
		}
	}
	for _, e := range evs {
		ok, err := s.emit(e)
		if err != nil {
			return res, err
		}
		if ok {
			res.published++
		}
	}
	return res, nil
}

func refusedNote(st costState, today string) string {
	var out []string
	for _, what := range []string{refusedCost, refusedAnomalies, refusedBudgets} {
		if st.Refused[what] == today {
			out = append(out, what+" not accessible to this role")
		}
	}
	return strings.Join(out, "; ")
}

func (s *Source) loadCost(account string) (costState, error) {
	st := costState{Days: map[string]float64{}, Refused: map[string]string{}}
	v, found, err := s.kv.Get(costGroup, account)
	if err != nil || !found {
		return st, err
	}
	if json.Unmarshal([]byte(v), &st) != nil {
		st = costState{} // unreadable: read again
	}
	if st.Days == nil {
		st.Days = map[string]float64{}
	}
	if st.Refused == nil {
		st.Refused = map[string]string{}
	}
	return st, nil
}

func (s *Source) saveCost(account string, st costState) error {
	b, err := json.Marshal(st)
	if err != nil {
		return err
	}
	return s.kv.Set(costGroup, account, string(b), 0)
}

func (st *costState) prune(now time.Time) {
	oldest := now.AddDate(0, 0, -costKeepDays).Format(dateLayout)
	for d := range st.Days {
		if d < oldest {
			delete(st.Days, d)
		}
	}
}

// trailingMean is the mean of the n days before date, over the days known.
func (st costState) trailingMean(date string, n int) float64 {
	d, err := time.Parse(dateLayout, date)
	if err != nil {
		return 0
	}
	sum, count := 0.0, 0
	for i := 1; i <= n; i++ {
		if v, ok := st.Days[d.AddDate(0, 0, -i).Format(dateLayout)]; ok {
			sum += v
			count++
		}
	}
	if count == 0 {
		return 0
	}
	return sum / float64(count)
}

// spike is a day costing more than the factor times its trailing mean and
// more than the floor. Today is judged on what it has cost so far, which
// only rises, so a day already past the rule is a spike before it is over.
func (s *Source) spike(r reader, st costState, date string, soFar bool) (sources.Event, bool) {
	amount, ok := st.Days[date]
	if !ok {
		return sources.Event{}, false
	}
	mean := st.trailingMean(date, s.cfg.TrailingDays)
	if amount <= s.cfg.SpikeFactor*mean || amount <= s.cfg.SpikeFloor {
		return sources.Event{}, false
	}
	unit := st.Unit
	when := "on " + date
	if soFar {
		when = "so far today (" + date + ")"
	}
	ratio := "with nothing spent in the days before"
	if mean > 0 {
		ratio = fmt.Sprintf("%.1f× the %d-day mean of %s", amount/mean, s.cfg.TrailingDays, money(mean, unit))
	}
	summary := fmt.Sprintf("Unblended cost %s was %s, %s. The rule: above %s× the trailing %d-day mean and above %s.",
		when, money(amount, unit), ratio, trimFloat(s.cfg.SpikeFactor), s.cfg.TrailingDays, money(s.cfg.SpikeFloor, unit))
	if mtd, from := monthToDate(st, date); mtd > 0 {
		summary += fmt.Sprintf(" Month to date (from %s): %s.", from, money(mtd, unit))
	}
	inspect := s.costInspect(r, date)
	summary += " See which services: " + inspect
	return sources.Event{
		Source:  SourceName,
		Account: r.label,
		Type:    "cost.anomaly",
		Key:     "cost-spike/" + r.account + "/" + date,
		Title:   fmt.Sprintf("AWS spend spike in %s: %s %s", r.label, money(amount, unit), when),
		Summary: summary,
		URL:     "https://us-east-1.console.aws.amazon.com/costmanagement/home#/cost-explorer",
		At:      s.now(),
		Urgent:  true,
		Data: map[string]any{
			"kind": "spike", "profile": r.profile.Name, "account_id": r.account, "date": date, "so_far": soFar,
			"amount": amount, "mean": mean, "factor": s.cfg.SpikeFactor, "floor": s.cfg.SpikeFloor, "unit": unit,
			"inspect": inspect,
		},
	}, true
}

// dailySummary is yesterday's spend, told quietly so memory keeps the trend.
// An account that spent nothing in the window is not told.
func (s *Source) dailySummary(r reader, st costState, now time.Time) (sources.Event, bool) {
	date := now.AddDate(0, 0, -1).Format(dateLayout)
	amount, ok := st.Days[date]
	if !ok {
		return sources.Event{}, false
	}
	unit := st.Unit
	week := map[string]float64{}
	var parts []string
	spent := false
	d, _ := time.Parse(dateLayout, date)
	for i := s.cfg.TrailingDays - 1; i >= 0; i-- {
		ds := d.AddDate(0, 0, -i).Format(dateLayout)
		v := st.Days[ds]
		week[ds] = v
		parts = append(parts, ds[5:]+" "+money(v, unit))
		if v >= 0.005 {
			spent = true
		}
	}
	mtd, from := monthToDate(st, date)
	if !spent && mtd < 0.005 {
		return sources.Event{}, false
	}
	mean := st.trailingMean(date, s.cfg.TrailingDays)
	summary := fmt.Sprintf("Unblended cost on %s: %s (%d-day mean before it %s). Month to date (%s to %s): %s. Last %d days: %s. "+
		"AWS finalises a day within about a day, so the latest figures can still rise.",
		date, money(amount, unit), s.cfg.TrailingDays, money(mean, unit), from, date, money(mtd, unit),
		s.cfg.TrailingDays, strings.Join(parts, ", "))
	return sources.Event{
		Source:  SourceName,
		Account: r.label,
		Type:    "cost.daily",
		Key:     "cost-daily/" + r.account + "/" + date,
		Title:   fmt.Sprintf("AWS spend in %s on %s: %s", r.label, date, money(amount, unit)),
		Summary: summary,
		URL:     "https://us-east-1.console.aws.amazon.com/costmanagement/home#/cost-explorer",
		At:      now,
		Data: map[string]any{
			"kind": "summary", "profile": r.profile.Name, "account_id": r.account, "date": date,
			"amount": amount, "mean": mean, "month_to_date": mtd, "unit": unit, "days": week,
			"inspect": s.costInspect(r, date),
		},
	}, true
}

// monthToDate sums date's month from its first day through date.
func monthToDate(st costState, date string) (float64, string) {
	first := date[:8] + "01"
	sum := 0.0
	for d, v := range st.Days {
		if d >= first && d <= date {
			sum += v
		}
	}
	return sum, first
}

func (s *Source) anomalyEvent(r reader, a Anomaly, now time.Time) sources.Event {
	end := a.End
	if end == "" {
		end = "ongoing"
	}
	summary := fmt.Sprintf("Cost Anomaly Detection found unexpected spend on %s from %s to %s: %s actual against %s expected (impact %s).",
		orDash(a.Dimension), orDash(a.Start), end, money(a.Actual, "USD"), money(a.Expected, "USD"), money(a.Impact, "USD"))
	if len(a.RootCauses) > 0 {
		summary += " Root causes: " + clip(strings.Join(a.RootCauses, "; "), 300) + "."
	}
	if a.Impact < s.cfg.SpikeFloor {
		summary += fmt.Sprintf(" Below the %s floor, so not urgent.", money(s.cfg.SpikeFloor, "USD"))
	}
	return sources.Event{
		Source:  SourceName,
		Account: r.label,
		Type:    "cost.anomaly",
		Key:     "cost-detector/" + r.account + "/" + a.ID,
		Title:   fmt.Sprintf("AWS cost anomaly in %s: %s unexpected on %s", r.label, money(a.Impact, "USD"), orDash(a.Dimension)),
		Summary: summary,
		URL:     "https://us-east-1.console.aws.amazon.com/costmanagement/home#/anomaly-detection/overview",
		At:      now,
		Urgent:  a.Impact >= s.cfg.SpikeFloor,
		Data: map[string]any{
			"kind": "detector", "profile": r.profile.Name, "account_id": r.account, "id": a.ID,
			"start": a.Start, "end": a.End, "dimension": a.Dimension, "root_causes": a.RootCauses,
			"impact": a.Impact, "actual": a.Actual, "expected": a.Expected, "score": a.Score, "monitor": a.MonitorARN,
		},
	}
}

func (s *Source) budgetEvent(r reader, b Budget, n BudgetNotification, now time.Time) sources.Event {
	threshold := trimFloat(n.Threshold) + "%"
	if n.ThresholdType == "ABSOLUTE_VALUE" {
		threshold = money(n.Threshold, b.Unit)
	}
	kind := "actual"
	if n.Type == "FORECASTED" {
		kind = "forecast"
	}
	period := budgetPeriod(b.TimeUnit, now)
	summary := fmt.Sprintf("Budget %s (%s): limit %s, actual %s, forecast %s. AWS reports its %s notification (%s %s) as crossed for %s.",
		b.Name, strings.ToLower(orDash(b.TimeUnit)), money(b.Limit, b.Unit), money(b.Actual, b.Unit), money(b.Forecast, b.Unit),
		kind, strings.ToLower(strings.ReplaceAll(n.Comparison, "_", " ")), threshold, period)
	return sources.Event{
		Source:  SourceName,
		Account: r.label,
		Type:    "cost.anomaly",
		Key:     fmt.Sprintf("cost-budget/%s/%s/%s/%s/%s", r.account, b.Name, n.Type, trimFloat(n.Threshold), period),
		Title:   fmt.Sprintf("AWS budget %s crossed its %s threshold (%s) in %s", b.Name, kind, threshold, r.label),
		Summary: summary,
		URL:     "https://us-east-1.console.aws.amazon.com/costmanagement/home#/budgets",
		At:      now,
		Urgent:  true,
		Data: map[string]any{
			"kind": "budget", "profile": r.profile.Name, "account_id": r.account, "budget": b.Name, "notification": n.Type,
			"threshold": n.Threshold, "threshold_type": n.ThresholdType, "limit": b.Limit, "actual": b.Actual,
			"forecast": b.Forecast, "unit": b.Unit, "period": period,
		},
	}
}

// budgetPeriod names the budget period now falls in, so a crossing is told
// once per period.
func budgetPeriod(unit string, now time.Time) string {
	switch unit {
	case "DAILY":
		return now.Format(dateLayout)
	case "QUARTERLY":
		return fmt.Sprintf("%d-Q%d", now.Year(), (int(now.Month())-1)/3+1)
	case "ANNUALLY":
		return fmt.Sprintf("%d", now.Year())
	default:
		return now.Format("2006-01")
	}
}

func (s *Source) costInspect(r reader, date string) string {
	d, err := time.Parse(dateLayout, date)
	if err != nil {
		return ""
	}
	return fmt.Sprintf("karmax tool call aws.read op=cost profile=%s start=%s end=%s",
		r.profile.Name, date, d.AddDate(0, 0, 1).Format(dateLayout))
}

func money(v float64, unit string) string {
	if unit == "" || unit == "USD" {
		return fmt.Sprintf("$%.2f", v)
	}
	return fmt.Sprintf("%.2f %s", v, unit)
}

func trimFloat(v float64) string {
	return strings.TrimSuffix(strings.TrimRight(fmt.Sprintf("%.4f", v), "0"), ".")
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
