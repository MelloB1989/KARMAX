package memory

import (
	"regexp"
	"strconv"
	"strings"
	"time"
)

// A loop's run, as its log records it.
//
// A loop used to file one entry per line it remembered, and every entry is a
// write that makes the store re-upload its whole repository: one night's loop
// lines were 343 of its 550 writes, chat-sweep alone 3.7 a run. So the runtime
// keeps a run's lines and files them as ONE entry when the run ends
// (LoopRunEntry), leaving out a line that says what the loop's previous run
// said (LogLineKey) — "still waiting" for the fifth time is not news.

// LoopRunEntry is one run's lines as a single entry in the loop's log, dated
// by when the run began. ok is false for a run that said nothing.
func LoopRunEntry(loop string, lines []string, began time.Time) (e MemoryEntry, ok bool) {
	var kept []string
	for _, l := range lines {
		if l = strings.TrimSpace(l); l != "" {
			kept = append(kept, l)
		}
	}
	if len(kept) == 0 {
		return MemoryEntry{}, false
	}
	e = LoopLog(loop, strings.Join(kept, "\n"))
	e.OccurredAt = began
	return e, true
}

// LogLineKey is what a line says without when it was said: a date or clock
// time that falls within the run (from to to, give or take two minutes, on
// the clock in loc or on UTC's) is masked, and case and spacing are folded.
//
// Loops stamp their lines — "(chat-sweep, 31 Oct 02:10) …" — so two runs that
// found exactly the same thing never wrote the same text. Only the run's own
// moment is masked: "the call moved to 4 Nov" and "… to 5 Nov" still differ,
// and so do two runs that say a meeting is at different times.
func LogLineKey(line string, from, to time.Time, loc *time.Location) string {
	w := runWindow(from, to, loc)
	s := maskEach(line, clockRe, func(m []string, before, after byte) bool {
		if isDigit(before) || before == ':' || isDigit(after) {
			return false
		}
		h, _ := strconv.Atoi(m[1])
		min, _ := strconv.Atoi(m[2])
		switch strings.ToLower(m[3]) {
		case "a":
			if h == 12 {
				h = 0
			}
		case "p":
			if h < 12 {
				h += 12
			}
		}
		return w.minutes[h*60+min]
	})
	s = maskEach(s, isoDateRe, func(m []string, before, after byte) bool {
		if isDigit(before) || isDigit(after) {
			return false
		}
		y, _ := strconv.Atoi(m[1])
		mo, _ := strconv.Atoi(m[2])
		d, _ := strconv.Atoi(m[3])
		return w.day(y, mo, d)
	})
	s = maskEach(s, dayMonthRe, func(m []string, before, after byte) bool {
		d, _ := strconv.Atoi(m[1])
		return !isDigit(before) && w.dayOf(monthOf(m[2]), d, m[3])
	})
	s = maskEach(s, monthDayRe, func(m []string, before, after byte) bool {
		d, _ := strconv.Atoi(m[2])
		return after != ':' && !isDigit(after) && w.dayOf(monthOf(m[1]), d, m[3])
	})
	return strings.Join(strings.Fields(strings.ToLower(s)), " ")
}

const months = `(jan(?:uary)?|feb(?:ruary)?|mar(?:ch)?|apr(?:il)?|may|june?|july?|aug(?:ust)?|sep(?:t(?:ember)?)?|oct(?:ober)?|nov(?:ember)?|dec(?:ember)?)`

var (
	clockRe    = regexp.MustCompile(`([0-2]?\d):([0-5]\d)(?::[0-5]\d(?:\.\d+)?)?(?:\s?([AaPp])\.?[Mm]\b\.?)?`)
	isoDateRe  = regexp.MustCompile(`(\d{4})-(\d{2})-(\d{2})`)
	dayMonthRe = regexp.MustCompile(`(?i)\b(\d{1,2})(?:st|nd|rd|th)?\s+` + months + `\b(?:\.?,?\s+(\d{4})\b)?`)
	monthDayRe = regexp.MustCompile(`(?i)\b` + months + `\.?\s+(\d{1,2})(?:st|nd|rd|th)?\b(?:,?\s+(\d{4})\b)?`)
)

// maskEach replaces each match keep approves with a placeholder. keep sees
// the submatches and the bytes either side of the match (0 at an edge).
func maskEach(s string, re *regexp.Regexp, keep func(m []string, before, after byte) bool) string {
	var b strings.Builder
	last := 0
	for _, idx := range re.FindAllStringSubmatchIndex(s, -1) {
		m := make([]string, len(idx)/2)
		for i := range m {
			if idx[2*i] >= 0 {
				m[i] = s[idx[2*i]:idx[2*i+1]]
			}
		}
		var before, after byte
		if idx[0] > 0 {
			before = s[idx[0]-1]
		}
		if idx[1] < len(s) {
			after = s[idx[1]]
		}
		if !keep(m, before, after) {
			continue
		}
		b.WriteString(s[last:idx[0]])
		b.WriteString("<when>")
		last = idx[1]
	}
	b.WriteString(s[last:])
	return b.String()
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

func monthOf(name string) int {
	n := strings.ToLower(name)
	for i, m := range []string{"jan", "feb", "mar", "apr", "may", "jun", "jul", "aug", "sep", "oct", "nov", "dec"} {
		if strings.HasPrefix(n, m) {
			return i + 1
		}
	}
	return 0
}

// window is the moments a run's own stamp can name.
type window struct {
	minutes map[int]bool    // minute of the day, h*60+m
	days    map[[3]int]bool // {year, month, day}
}

// runWindow is every minute and date from-2m … to+2m, on loc's clock and UTC's.
func runWindow(from, to time.Time, loc *time.Location) window {
	if loc == nil {
		loc = time.UTC
	}
	w := window{minutes: map[int]bool{}, days: map[[3]int]bool{}}
	start, end := from.Add(-2*time.Minute), to.Add(2*time.Minute)
	if end.Before(start) {
		end = start
	}
	for t, n := start.Truncate(time.Minute), 0; !t.After(end) && n < 24*60; t, n = t.Add(time.Minute), n+1 {
		for _, z := range []*time.Location{loc, time.UTC} {
			at := t.In(z)
			w.minutes[at.Hour()*60+at.Minute()] = true
			w.days[[3]int{at.Year(), int(at.Month()), at.Day()}] = true
		}
	}
	return w
}

func (w window) day(y, m, d int) bool { return w.days[[3]int{y, m, d}] }

// dayOf matches a day and month, and the year when one is given.
func (w window) dayOf(m, d int, year string) bool {
	if year != "" {
		y, _ := strconv.Atoi(year)
		return w.day(y, m, d)
	}
	for k := range w.days {
		if k[1] == m && k[2] == d {
			return true
		}
	}
	return false
}
