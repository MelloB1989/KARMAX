package aws

import (
	"fmt"
	"strings"
)

// Summary is the whole source's standing in one line: healthy while every
// profile reads (or another reads for it), degraded while some logins are
// out, failed when nothing can be read, naming each login that would help.
func Summary(hs []Health) (status, detail string) {
	if len(hs) == 0 {
		return "not_configured", "no AWS profiles in the shared config"
	}
	read, failed, degraded := 0, 0, 0
	seen := map[string]bool{}
	var logins []string
	for _, h := range hs {
		switch h.Status {
		case StatusHealthy:
			read++
		case StatusDegraded:
			read++ // logged in; something it should read could not be
			degraded++
		case StatusFailed:
			failed++
			if h.Fix != "" && !seen[h.Fix] {
				seen[h.Fix] = true
				logins = append(logins, h.Fix)
			}
		}
	}
	accounts := fmt.Sprintf("%d account read", read)
	if read != 1 {
		accounts = fmt.Sprintf("%d accounts read", read)
	}
	switch {
	case read == 0 && failed > 0:
		status, accounts = StatusFailed, "no account read"
	case failed > 0 || degraded > 0:
		status = StatusDegraded
	default:
		status = StatusHealthy
	}
	detail = accounts
	if failed > 0 {
		detail += fmt.Sprintf("; %d profile(s) not logged in: %s", failed, strings.Join(logins, "; "))
	}
	if degraded > 0 {
		detail += fmt.Sprintf("; %d with something unreadable (see aws.read op=profiles)", degraded)
	}
	return status, detail
}

// NewFixes are the logins needed now that were not needed before.
func NewFixes(prev, cur []string) []string {
	had := make(map[string]bool, len(prev))
	for _, f := range prev {
		had[f] = true
	}
	var out []string
	for _, f := range cur {
		if !had[f] {
			out = append(out, f)
		}
	}
	return out
}
