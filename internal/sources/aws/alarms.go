package aws

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/MelloB1989/karmax/internal/sources"
)

// historyOverlap is how far before the last read each history read starts
// again, so an entry CloudWatch records a little late is not missed. What it
// reads twice is published once.
const historyOverlap = 10 * time.Minute

const (
	alarmCursorGroup = "aws-alarm-cursor"
	alarmStateGroup  = "aws-alarm-state"
)

// AlarmReport is what one alarm poll did.
type AlarmReport struct {
	Readers   int      // account/regions read
	Alarms    int      // alarms seen
	InAlarm   int      // of those, in ALARM now
	Published int      // events published
	Fixes     []string // login commands that would bring skipped profiles back
}

// PollAlarms reads every account and region once and publishes the alarms
// that went into ALARM or came back to OK since the last read.
//
// It fails only when nothing could be read for a reason other than logins:
// expired logins are the ordinary state of SSO and live in health, while an
// account that cannot be reached at all is an outage the loop must show.
func (s *Source) PollAlarms(ctx context.Context) (AlarmReport, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var rep AlarmReport
	readers, health, err := s.plan(ctx, true)
	if err != nil {
		return rep, err
	}
	now := s.now()
	ok, broken := 0, 0
	var lastErr error
	for _, h := range health {
		if h.Status == StatusDegraded {
			broken++
		}
	}
	for _, r := range readers {
		res, err := s.pollAlarmsOf(ctx, r)
		h := health[r.profile.Name]
		if err != nil {
			if isAuth(err) {
				h = failure(r.profile, err)
				h.Account = r.label
			} else {
				h.Status, h.Detail = StatusDegraded, "alarms: "+clip(err.Error(), 240)
				broken++
				lastErr = err
			}
			health[r.profile.Name] = h
			continue
		}
		ok++
		rep.Readers++
		rep.Alarms += res.alarms
		rep.InAlarm += res.inAlarm
		rep.Published += res.published
		h.Detail = joinDetail(fmt.Sprintf("%d alarms, %d in ALARM", res.alarms, res.inAlarm), res.note)
		health[r.profile.Name] = h
	}
	for name, h := range health {
		h.CheckedAt = now
		health[name] = h
	}
	s.health = health
	rep.Fixes = fixes(health)
	if ok == 0 && broken > 0 {
		if lastErr == nil {
			lastErr = fmt.Errorf("every profile failed; see health")
		}
		return rep, fmt.Errorf("no AWS account could be read: %w", lastErr)
	}
	return rep, nil
}

type alarmResult struct {
	alarms, inAlarm, published int
	note                       string
}

// pollAlarmsOf reads one account and region.
func (s *Source) pollAlarmsOf(ctx context.Context, r reader) (alarmResult, error) {
	var res alarmResult
	alarms, err := r.api.Alarms(ctx)
	if err != nil {
		return res, err
	}
	byName := make(map[string]*Alarm, len(alarms))
	for i := range alarms {
		byName[alarms[i].Name] = &alarms[i]
		if alarms[i].State == "ALARM" {
			res.inAlarm++
		}
	}
	res.alarms = len(alarms)

	now := s.now()
	cursorKey := r.account + "/" + r.region
	from := now.Add(-s.cfg.AlarmLookback)
	firstSight := true
	if v, found, err := s.kv.Get(alarmCursorGroup, cursorKey); err != nil {
		return res, err
	} else if found {
		if t, err := time.Parse(time.RFC3339Nano, v); err == nil {
			from, firstSight = t.Add(-historyOverlap), false
		}
	}

	hist, err := r.api.AlarmHistory(ctx, from, now)
	switch {
	case err == nil:
		n, err := s.publishHistory(r, hist, byName, firstSight, now)
		res.published += n
		if err != nil {
			return res, err
		}
	case isDenied(err):
		res.note = "alarm history not readable (cloudwatch:DescribeAlarmHistory), comparing states instead"
		n, err := s.compareStates(r, alarms, now, firstSight)
		res.published += n
		if err != nil {
			return res, err
		}
	default:
		return res, err
	}
	return res, s.kv.Set(alarmCursorGroup, cursorKey, now.UTC().Format(time.RFC3339Nano), 0)
}

// publishHistory publishes each transition into ALARM, and each from ALARM
// back to OK. A transition into ALARM is urgent only while it still holds:
// the alarm's latest change, and the alarm in ALARM now.
func (s *Source) publishHistory(r reader, hist []Transition, byName map[string]*Alarm, firstSight bool, now time.Time) (int, error) {
	latest := map[string]time.Time{}
	for _, t := range hist {
		if t.At.After(latest[t.Alarm]) {
			latest[t.Alarm] = t.At
		}
	}
	told := map[string]bool{}
	n := 0
	for _, t := range hist {
		switch {
		case t.To == "ALARM":
		case t.To == "OK" && t.From == "ALARM":
		default:
			continue // data arriving or going missing is not news
		}
		a := byName[t.Alarm]
		stillFiring := a != nil && a.State == "ALARM" && !t.At.Before(latest[t.Alarm])
		if t.To == "ALARM" {
			told[t.Alarm] = true
		}
		ok, err := s.emit(asBacklog(s.alarmEvent(r, t, a, t.To == "ALARM" && stillFiring), firstSight))
		if err != nil {
			return n, err
		}
		if ok {
			n++
		}
	}
	if !firstSight {
		return n, nil
	}
	// The first read of an account: alarms already firing before the
	// lookback are told once, as context rather than news.
	for _, a := range byName {
		if a.State != "ALARM" || told[a.Name] {
			continue
		}
		t := Transition{Alarm: a.Name, Composite: a.Composite, At: a.UpdatedAt, To: "ALARM", Reason: a.Reason}
		recent := now.Sub(a.UpdatedAt) <= s.cfg.AlarmLookback
		ok, err := s.emit(asBacklog(s.alarmEvent(r, t, a, recent), firstSight))
		if err != nil {
			return n, err
		}
		if ok {
			n++
		}
	}
	return n, nil
}

// compareStates finds transitions without alarm history, by comparing each
// alarm with how the last read left it. Used when the role may not read
// history; it misses an alarm that went off and recovered between two reads.
func (s *Source) compareStates(r reader, alarms []Alarm, now time.Time, firstSight bool) (int, error) {
	n := 0
	for i := range alarms {
		a := &alarms[i]
		key := r.account + "/" + r.region + "/" + a.Name
		cur := a.State + "|" + a.UpdatedAt.UTC().Format(time.RFC3339Nano)
		prev, found, err := s.kv.Get(alarmStateGroup, key)
		if err != nil {
			return n, err
		}
		if found && prev == cur {
			continue
		}
		prevState, _, _ := strings.Cut(prev, "|")
		var t *Transition
		switch {
		case a.State == "ALARM": // new to this read, or changed since the last
			t = &Transition{Alarm: a.Name, At: a.UpdatedAt, From: prevState, To: "ALARM", Reason: a.Reason}
		case a.State == "OK" && found && prevState == "ALARM":
			t = &Transition{Alarm: a.Name, At: a.UpdatedAt, From: "ALARM", To: "OK", Reason: a.Reason}
		}
		if t != nil {
			urgent := t.To == "ALARM" && now.Sub(a.UpdatedAt) <= s.cfg.AlarmLookback
			ok, err := s.emit(asBacklog(s.alarmEvent(r, *t, a, urgent), firstSight))
			if err != nil {
				return n, err
			}
			if ok {
				n++
			}
		}
		if err := s.kv.Set(alarmStateGroup, key, cur, 0); err != nil {
			return n, err
		}
	}
	return n, nil
}

// alarmEvent is a transition as the decision core reads it. a is nil when
// the alarm has since been deleted.
func (s *Source) alarmEvent(r reader, t Transition, a *Alarm, urgent bool) sources.Event {
	from := t.From
	if from == "" {
		from = "?"
	}
	var b strings.Builder
	if t.From != "" {
		fmt.Fprintf(&b, "%s went %s → %s at %s.", t.Alarm, from, t.To, t.At.UTC().Format("2006-01-02 15:04 UTC"))
	} else {
		fmt.Fprintf(&b, "%s is %s, since %s.", t.Alarm, t.To, t.At.UTC().Format("2006-01-02 15:04 UTC"))
	}
	if a != nil {
		if m := describeMetric(a); m != "" {
			b.WriteString(" " + m)
		}
	}
	reason := t.Reason
	if reason == "" && a != nil && a.State == t.To {
		reason = a.Reason
	}
	if reason != "" {
		b.WriteString(" Reason: " + clip(reason, 400))
	}
	if t.To == "ALARM" && !urgent && a != nil && a.State != "ALARM" {
		b.WriteString(" It has since recovered (now " + a.State + ").")
	}
	if a != nil && a.Description != "" {
		b.WriteString(" Description: " + clip(a.Description, 300))
	}

	title := fmt.Sprintf("AWS alarm %s is ALARM in %s", t.Alarm, r.label)
	if t.To == "OK" {
		title = fmt.Sprintf("AWS alarm %s is back to OK in %s", t.Alarm, r.label)
	}
	data := map[string]any{
		"profile": r.profile.Name, "account_id": r.account, "region": r.region,
		"alarm": t.Alarm, "state": t.To, "previous": t.From, "reason": reason,
		"inspect": fmt.Sprintf("karmax tool call aws.read op=alarm profile=%s region=%s alarm=%q", r.profile.Name, r.region, t.Alarm),
	}
	if a != nil {
		data["arn"] = a.ARN
		data["state_now"] = a.State
		if a.Metric != "" {
			data["namespace"], data["metric"], data["statistic"], data["period"] = a.Namespace, a.Metric, a.Statistic, a.Period
			dims := map[string]string{}
			for _, d := range a.Dimensions {
				dims[d.Name] = d.Value
			}
			data["dimensions"] = dims
		}
		if a.Threshold != nil {
			data["threshold"], data["comparison"] = *a.Threshold, a.Comparison
		}
	}
	return sources.Event{
		Source:  SourceName,
		Account: r.label,
		Type:    "alarm." + t.To,
		Key:     fmt.Sprintf("alarm/%s/%s/%s/%s/%d", r.account, r.region, t.Alarm, t.To, t.At.UnixMilli()),
		Title:   title,
		Summary: b.String(),
		URL:     alarmURL(r.region, t.Alarm),
		At:      t.At,
		Urgent:  urgent,
		Data:    data,
	}
}

// describeMetric says what an alarm watches and where it trips.
func describeMetric(a *Alarm) string {
	switch {
	case a.Composite && a.Rule != "":
		return "Composite rule: " + clip(a.Rule, 300) + "."
	case a.Metric != "":
		var dims []string
		for _, d := range a.Dimensions {
			dims = append(dims, d.Name+"="+d.Value)
		}
		m := "Metric: " + strings.TrimSpace(a.Namespace+" "+a.Metric)
		if len(dims) > 0 {
			m += " (" + strings.Join(dims, ", ") + ")"
		}
		if a.Statistic != "" {
			m += ", " + a.Statistic
		}
		if a.Period > 0 {
			m += fmt.Sprintf(" over %ds", a.Period)
		}
		if a.EvaluationPeriods > 1 {
			m += fmt.Sprintf(" for %d periods", a.EvaluationPeriods)
		}
		if a.Threshold != nil {
			m += ", " + a.Comparison + " " + strconv.FormatFloat(*a.Threshold, 'f', -1, 64)
		}
		return m + "."
	case len(a.Expressions) > 0:
		m := "Metric math: " + clip(strings.Join(a.Expressions, "; "), 300)
		if a.Threshold != nil {
			m += ", " + a.Comparison + " " + strconv.FormatFloat(*a.Threshold, 'f', -1, 64)
		}
		return m + "."
	}
	return ""
}

// alarmURL opens the alarm in the console of whichever account is signed in.
func alarmURL(region, name string) string {
	return fmt.Sprintf("https://%s.console.aws.amazon.com/cloudwatch/home?region=%s#alarmsV2:alarm/%s",
		region, region, url.PathEscape(name))
}

// asBacklog marks what the first read of an account finds. It happened before
// KARMAX was watching, so it reaches the agent as context and never pages the
// operator, however urgent it is.
func asBacklog(e sources.Event, firstSight bool) sources.Event {
	e.Backlog = firstSight
	return e
}
