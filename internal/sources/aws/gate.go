package aws

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/MelloB1989/karmax/internal/sources"
)

// The gate stands between what an alarm poll finds and what it publishes, so
// that an alarm flapping OK -> ALARM -> OK does not wake a model turn on every
// flip. Three rules, each in Config:
//
//   - Debounce: an alarm's ALARM is published once it has been in ALARM at
//     AlarmDebounce consecutive polls, and its recovery only if its ALARM was.
//     An ALARM that is over sooner publishes nothing; it is counted.
//   - Cooldown: after an alarm's ALARM is published, its next ALARMs are held
//     for AlarmCooldown, unless one stays in ALARM for cooldownBreakthrough
//     times the debounce. The next ALARM published says how many were held.
//   - Ignore: alarms whose names match AlarmIgnore are never published.
//
// What is never held: the first read of an account (already marked backlog),
// an alarm the gate has not seen before, and anything about an alarm whose
// ALARM has been published, until it is back to OK; INSUFFICIENT_DATA does
// not count as back. Its state lives in the KV store, one record per account
// and region, so a restart neither forgets an ALARM in progress nor tells one
// twice.

const alarmGateGroup = "aws-alarm-gate"

// cooldownBreakthrough is how many debounces an alarm must stay in ALARM to
// be published inside its cooldown: a flap is held, an outage is not.
const cooldownBreakthrough = 4

// HeldAlarm is an alarm whose flips the gate held back, with the count since
// the alarm was last told.
type HeldAlarm struct {
	Alarm string
	Count int
}

// gateState is one alarm's standing with the gate.
type gateState struct {
	Streak int         `json:"streak,omitempty"` // consecutive polls that ended with it in ALARM
	Told   bool        `json:"told,omitempty"`   // its ALARM is published, its recovery not yet
	ToldAt time.Time   `json:"told_at,omitzero"` // when an ALARM of it was last published
	Last   time.Time   `json:"last,omitzero"`    // the latest transition taken in: history is read with overlap
	Open   *Transition `json:"open,omitempty"`   // the ALARM this episode began with, not yet published
	Held   int         `json:"held,omitempty"`   // ALARMs held back since it was last told
}

func (s *Source) debounce() int { return max(s.cfg.AlarmDebounce, 1) }

// cooling reports whether an alarm told recently is inside its cooldown.
func (s *Source) cooling(st *gateState, now time.Time) bool {
	return s.cfg.AlarmCooldown > 0 && !st.ToldAt.IsZero() && now.Sub(st.ToldAt) < s.cfg.AlarmCooldown
}

// gate publishes what this poll's candidates and the alarms' states now
// call for, and records where every alarm stands. alarms are this reader's
// alarms, the ignored ones already left out.
func (s *Source) gate(r reader, alarms []Alarm, cands []candidate, firstSight bool, now time.Time) (int, []HeldAlarm, error) {
	key := r.account + "/" + r.region
	states := map[string]*gateState{}
	v, found, err := s.kv.Get(alarmGateGroup, key)
	if err != nil {
		return 0, nil, err
	}
	if found {
		if err := json.Unmarshal([]byte(v), &states); err != nil {
			found = false // unreadable: start again as on the first poll with the gate
			states = map[string]*gateState{}
		}
	}
	// The first poll with the gate (the account's first read, or the first
	// since the gate shipped) publishes as before it existed and records
	// where every alarm stands.
	fresh := firstSight || !found

	n := 0
	emit := func(e sources.Event) error {
		ok, err := s.emit(e)
		if ok {
			n++
		}
		return err
	}
	heldBefore := map[string]int{}
	for name, st := range states {
		heldBefore[name] = st.Held
	}
	told := map[string]bool{} // an ALARM published as before the gate, this poll
	latest := map[string]time.Time{}

	for _, c := range cands {
		name := c.t.Alarm
		if c.t.At.After(latest[name]) {
			latest[name] = c.t.At
		}
		st, known := states[name]
		if fresh || !known {
			if err := emit(c.ev); err != nil {
				return n, nil, err
			}
			if c.t.To == "ALARM" {
				told[name] = true
			}
			continue
		}
		if !c.t.At.After(st.Last) {
			continue // read again through the history overlap
		}
		st.Last = c.t.At
		if st.Told {
			if err := emit(c.ev); err != nil {
				return n, nil, err
			}
			if c.t.To == "OK" {
				st.Told = false
			}
			continue
		}
		switch {
		case c.t.To == "ALARM":
			st.Open, st.Streak = &c.t, 0 // a new episode: its polls count from this one
			if s.debounce() == 1 && !s.cooling(st, now) {
				if err := emit(s.withHeld(c.ev, st)); err != nil {
					return n, nil, err
				}
				st.tell(now)
			}
		case c.t.From == "ALARM" && st.Open != nil:
			st.Open = nil
			st.Held++
		}
	}

	next := make(map[string]*gateState, len(alarms))
	for i := range alarms {
		a := &alarms[i]
		st, known := states[a.Name]
		if fresh || !known {
			st = &gateState{Last: latest[a.Name]}
			if a.State == "ALARM" {
				st.Streak = 1
				switch {
				case fresh:
					st.Told, st.ToldAt = true, a.UpdatedAt // told before the gate, or as backlog now
				case told[a.Name]:
					st.tell(now)
				default:
					// New to the gate and in ALARM with nothing in its history:
					// told now, as an ALARM is.
					t := Transition{Alarm: a.Name, Composite: a.Composite, At: a.UpdatedAt, To: "ALARM", Reason: a.Reason}
					recent := now.Sub(a.UpdatedAt) <= s.cfg.AlarmLookback
					if err := emit(s.alarmEvent(r, t, a, recent)); err != nil {
						return n, nil, err
					}
					st.tell(now)
				}
			}
			next[a.Name] = st
			continue
		}
		next[a.Name] = st
		if a.State != "ALARM" {
			st.Streak = 0
			if st.Told && a.State == "OK" {
				// Back by way of INSUFFICIENT_DATA, which is no transition of
				// its own: the recovery of an ALARM that was told is told.
				t := Transition{Alarm: a.Name, Composite: a.Composite, At: a.UpdatedAt, To: "OK", Reason: a.Reason}
				if err := emit(s.alarmEvent(r, t, a, false)); err != nil {
					return n, nil, err
				}
				st.Told = false
			}
			if st.Open != nil && !st.Told { // over without a transition read: state comparison, or history late
				st.Open = nil
				st.Held++
			}
			continue
		}
		st.Streak++
		if st.Told {
			continue
		}
		if st.Open == nil { // in ALARM with no transition read for it
			st.Open = &Transition{Alarm: a.Name, Composite: a.Composite, At: a.UpdatedAt, To: "ALARM", Reason: a.Reason}
		}
		if st.Streak < s.debounce() || (s.cooling(st, now) && st.Streak < cooldownBreakthrough*s.debounce()) {
			continue
		}
		urgent := now.Sub(st.Open.At) <= s.cfg.AlarmLookback
		if err := emit(s.withHeld(s.alarmEvent(r, *st.Open, a, urgent), st)); err != nil {
			return n, nil, err
		}
		st.tell(now)
	}

	b, err := json.Marshal(next)
	if err != nil {
		return n, nil, err
	}
	if err := s.kv.Set(alarmGateGroup, key, string(b), 0); err != nil {
		return n, nil, err
	}
	var held []HeldAlarm
	for name, st := range next {
		if st.Held > heldBefore[name] {
			held = append(held, HeldAlarm{Alarm: name, Count: st.Held})
		}
	}
	sort.Slice(held, func(i, j int) bool { return held[i].Alarm < held[j].Alarm })
	return n, held, nil
}

// tell records an ALARM published.
func (st *gateState) tell(now time.Time) {
	st.Told, st.ToldAt, st.Open, st.Held = true, now, nil, 0
}

// withHeld folds the flips held back since the last report into the event.
func (s *Source) withHeld(e sources.Event, st *gateState) sources.Event {
	if st.Held == 0 {
		return e
	}
	times := "times"
	if st.Held == 1 {
		times = "time"
	}
	e.Summary += fmt.Sprintf(" It has flipped %d %s since the last report.", st.Held, times)
	data := make(map[string]any, len(e.Data)+1)
	for k, v := range e.Data {
		data[k] = v
	}
	data["flips_since_report"] = st.Held
	e.Data = data
	return e
}

// ignored reports whether the ignore list names an alarm.
func (s *Source) ignored(name string) bool {
	for _, re := range s.ignore {
		if re.MatchString(name) {
			return true
		}
	}
	return false
}

// globs compiles ignore patterns: * is any run of characters, slashes
// included (target-tracking alarm names have them), and ? is one character.
func globs(patterns []string) []*regexp.Regexp {
	var out []*regexp.Regexp
	for _, p := range patterns {
		if p = strings.TrimSpace(p); p == "" {
			continue
		}
		var b strings.Builder
		b.WriteString("^")
		for _, r := range p {
			switch r {
			case '*':
				b.WriteString(".*")
			case '?':
				b.WriteString(".")
			default:
				b.WriteString(regexp.QuoteMeta(string(r)))
			}
		}
		b.WriteString("$")
		out = append(out, regexp.MustCompile(b.String()))
	}
	return out
}
