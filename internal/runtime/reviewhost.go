package runtime

import (
	"strconv"
	"strings"
	"time"
)

// reviewExpireAfter reads how long a memory-review question waits for an
// answer before it expires, from KARMAX_REVIEW_EXPIRE_AFTER: a number of days
// ("10d") or a Go duration ("36h"); "off", "never" or "0" never expires one.
// Unset or unreadable is 0, which the reviewer takes as its default — an
// unreadable setting must not quietly turn expiry off and bring the stall back.
func reviewExpireAfter(getenv func(string) string) time.Duration {
	v := strings.ToLower(strings.TrimSpace(getenv("KARMAX_REVIEW_EXPIRE_AFTER")))
	switch v {
	case "":
		return 0
	case "off", "never", "0":
		return -1
	}
	if days, ok := strings.CutSuffix(v, "d"); ok {
		if n, err := strconv.Atoi(days); err == nil && n > 0 {
			return time.Duration(n) * 24 * time.Hour
		}
		return 0
	}
	if d, err := time.ParseDuration(v); err == nil && d > 0 {
		return d
	}
	return 0
}
