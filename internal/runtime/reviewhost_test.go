package runtime

import (
	"testing"
	"time"
)

func TestReviewExpiryIsReadFromTheEnvironment(t *testing.T) {
	for in, want := range map[string]time.Duration{
		"":        0, // the reviewer's default
		"10d":     10 * 24 * time.Hour,
		" 3d ":    3 * 24 * time.Hour,
		"36h":     36 * time.Hour,
		"off":     -1,
		"never":   -1,
		"0":       -1,
		"soonish": 0, // unreadable: the default, not "never"
		"-2d":     0,
	} {
		got := reviewExpireAfter(func(k string) string {
			if k == "KARMAX_REVIEW_EXPIRE_AFTER" {
				return in
			}
			return ""
		})
		if got != want {
			t.Errorf("KARMAX_REVIEW_EXPIRE_AFTER=%q: %v, want %v", in, got, want)
		}
	}
}
