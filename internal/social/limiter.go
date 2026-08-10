package social

import (
	"fmt"
	"strings"
	"time"
)

// Recorder is the persistence a Limiter needs. An interface rather than the
// store itself so the limiter can be tested without a database, and so the
// package that enforces the rules does not depend on the one that stores them.
type Recorder interface {
	RecordPost(platform, status, postID, text, detail string) error
	CountPostsSince(platform string, since time.Time) (int, error)
	LastPostAt(platform string) (time.Time, error)
}

// Limiter bounds how much KARMAX may say in the operator's name.
//
// The limits are not about API quotas — X and LinkedIn would happily take
// twenty posts a day. They are about the failure this design actually has:
// nobody reads these before they go out, so a loop that decides everything is
// worth posting produces a feed nobody wants to be the author of, and a bug
// that fires the workflow repeatedly produces it within the hour.
type Limiter struct {
	Rec Recorder
	// PerDay is how many posts a platform may receive in 24 hours.
	PerDay int
	// MinGap is the minimum spacing between posts on one platform, so a burst
	// cannot spend the day's whole allowance in a minute.
	MinGap time.Duration
	// Disabled is the kill switch, read at post time rather than at startup so
	// turning it off takes effect on the next post rather than the next restart.
	Disabled func() (bool, string)
}

// DefaultPerDay and DefaultMinGap are what an operator gets without saying
// anything: two posts a day, at least three hours apart. A person posts about
// that often; more reads as automation, which defeats the point.
const (
	DefaultPerDay = 2
	DefaultMinGap = 3 * time.Hour
)

// Allow reports whether something may be posted to a platform now.
func (l *Limiter) Allow(platform string) error {
	if l == nil || l.Rec == nil {
		return nil
	}
	if l.Disabled != nil {
		if off, why := l.Disabled(); off {
			if why == "" {
				why = "posting is switched off"
			}
			return &Refusal{Reason: why}
		}
	}

	perDay, minGap := l.PerDay, l.MinGap
	if perDay <= 0 {
		perDay = DefaultPerDay
	}
	if minGap <= 0 {
		minGap = DefaultMinGap
	}

	// A storage failure blocks the post. The alternative — posting because the
	// count could not be read — spends an unbounded budget precisely when
	// something is already wrong.
	n, err := l.Rec.CountPostsSince(platform, time.Now().Add(-24*time.Hour))
	if err != nil {
		return fmt.Errorf("social: cannot tell how much has been posted today, so nothing is: %w", err)
	}
	if n >= perDay {
		return &Refusal{Reason: fmt.Sprintf(
			"%s already had %d posts in the last 24 hours, which is the limit", platform, n)}
	}

	last, err := l.Rec.LastPostAt(platform)
	if err != nil {
		return fmt.Errorf("social: cannot tell when %s was last posted to, so nothing is: %w", platform, err)
	}
	if !last.IsZero() {
		if wait := minGap - time.Since(last); wait > 0 {
			return &Refusal{Reason: fmt.Sprintf(
				"the last %s post was %s ago; posts are spaced at least %s apart, so this one is %s early",
				platform, round(time.Since(last)), round(minGap), round(wait))}
		}
	}
	return nil
}

// Record writes the outcome, whatever it was.
func (l *Limiter) Record(platform, status, postID, text string, cause error) {
	if l == nil || l.Rec == nil {
		return
	}
	detail := ""
	if cause != nil {
		detail = cause.Error()
	}
	_ = l.Rec.RecordPost(platform, status, postID, text, detail)
}

// Status describes an error for the log: refused by the rules, or failed.
func Status(err error) string {
	if err == nil {
		return "posted"
	}
	var r *Refusal
	if asRefusal(err, &r) {
		return "refused"
	}
	return "failed"
}

func asRefusal(err error, target **Refusal) bool {
	for err != nil {
		if r, ok := err.(*Refusal); ok {
			*target = r
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

// round makes a duration readable in a sentence: "2h14m", not "2h14m3.918s".
func round(d time.Duration) string {
	if d >= time.Hour {
		return strings.TrimSuffix(d.Round(time.Minute).String(), "0s")
	}
	return d.Round(time.Minute).String()
}
