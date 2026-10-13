package orgops

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// The threat scan runs over the lines a PR ADDS. It is the cheap, certain half
// of security review: a leaked key has a shape, and finding it costs a regexp,
// not a model call. Whatever needs judgement is left to the model review.

type rule struct {
	name     string
	severity Severity
	re       *regexp.Regexp
}

var builtinRules = []PatternRule{
	{Name: "aws-access-key", Severity: "blocker", Pattern: `\b(AKIA|ASIA)[0-9A-Z]{16}\b`},
	{Name: "github-token", Severity: "blocker", Pattern: `\b(ghp|gho|ghu|ghs|ghr)_[A-Za-z0-9]{36,}\b|\bgithub_pat_[A-Za-z0-9_]{40,}\b`},
	{Name: "slack-token", Severity: "blocker", Pattern: `\bxox[abprs]-[A-Za-z0-9-]{10,}\b`},
	{Name: "private-key", Severity: "blocker", Pattern: `-----BEGIN ([A-Z]+ )?PRIVATE KEY-----`},
	{Name: "stripe-key", Severity: "blocker", Pattern: `\b(sk|rk)_live_[A-Za-z0-9]{20,}\b`},
	{Name: "google-api-key", Severity: "blocker", Pattern: `\bAIza[0-9A-Za-z_\-]{35}\b`},
	{Name: "anthropic-or-openai-key", Severity: "blocker", Pattern: `\bsk-(ant-)?[A-Za-z0-9_\-]{32,}\b`},
	{Name: "hardcoded-credential", Severity: "warning", Pattern: `(?i)\b(password|passwd|secret|api_?key|token)\b\s*[:=]\s*["'][^"'\s]{8,}["']`},
	{Name: "db-url-with-password", Severity: "blocker", Pattern: `\b(postgres|postgresql|mysql|mongodb(\+srv)?|redis)://[^:\s/]+:[^@\s]{3,}@`},
	{Name: "tls-verify-disabled", Severity: "warning", Pattern: `InsecureSkipVerify:\s*true|verify\s*=\s*False|rejectUnauthorized:\s*false|NODE_TLS_REJECT_UNAUTHORIZED`},
	{Name: "sql-string-concat", Severity: "warning", Pattern: `(?i)(SELECT|INSERT|UPDATE|DELETE)\s.+["']\s*\+\s*\w+|fmt\.Sprintf\(\s*"(?i:SELECT|INSERT|UPDATE|DELETE)[^"]*%[sv]`},
	{Name: "shell-injection", Severity: "warning", Pattern: `exec\.Command\(\s*"(sh|bash)"\s*,\s*"-c"|subprocess\.\w+\([^)]*shell\s*=\s*True|\beval\(`},
	{Name: "wide-open-cors", Severity: "warning", Pattern: `Access-Control-Allow-Origin["']?\s*[:,]\s*["']\*`},
}

type scanner struct {
	rules []rule
	allow []*regexp.Regexp
}

func newScanner(c ThreatConfig) (*scanner, error) {
	s := &scanner{}
	if c.Disabled {
		return s, nil
	}
	for _, r := range append(append([]PatternRule{}, builtinRules...), c.ExtraPatterns...) {
		re, err := regexp.Compile(r.Pattern)
		if err != nil {
			return nil, fmt.Errorf("org_ops.threats: pattern %q: %w", r.Name, err)
		}
		sev := Severity(strings.ToLower(r.Severity))
		if sev.rank() == 0 && sev != SevInfo {
			sev = SevWarning
		}
		s.rules = append(s.rules, rule{name: r.Name, severity: sev, re: re})
	}
	for _, a := range c.Allow {
		re, err := regexp.Compile(a)
		if err != nil {
			return nil, fmt.Errorf("org_ops.threats.allow: %w", err)
		}
		s.allow = append(s.allow, re)
	}
	return s, nil
}

// ScanDiff reports risky lines added by a PR's files.
func ScanDiff(c ThreatConfig, files []PRFile) []Finding {
	s, err := newScanner(c)
	if err != nil {
		return []Finding{{Rule: "config", Severity: SevWarning, Message: err.Error()}}
	}
	var out []Finding
	for _, f := range files {
		for _, l := range addedLines(f.Patch) {
			out = append(out, s.scanLine(f.Filename, l.n, l.text)...)
		}
	}
	return out
}

func (s *scanner) scanLine(file string, n int, text string) []Finding {
	for _, a := range s.allow {
		if a.MatchString(text) || a.MatchString(file) {
			return nil
		}
	}
	var out []Finding
	for _, r := range s.rules {
		if r.re.MatchString(text) {
			out = append(out, Finding{Rule: r.name, Severity: r.severity, Path: file, Line: n,
				Message: fmt.Sprintf("%s at %s:%d — %s", r.name, file, n, redact(strings.TrimSpace(text)))})
		}
	}
	return out
}

type line struct {
	n    int
	text string
}

var hunkHeader = regexp.MustCompile(`^@@ -\d+(?:,\d+)? \+(\d+)(?:,\d+)? @@`)

// addedLines walks a unified diff and returns each added line with its number
// in the new file, which is what an inline review comment is anchored to.
func addedLines(patch string) []line {
	var out []line
	n := 0
	for _, raw := range strings.Split(patch, "\n") {
		if m := hunkHeader.FindStringSubmatch(raw); m != nil {
			n, _ = strconv.Atoi(m[1])
			continue
		}
		switch {
		case strings.HasPrefix(raw, "+++"), strings.HasPrefix(raw, "---"):
		case strings.HasPrefix(raw, "+"):
			out = append(out, line{n: n, text: raw[1:]})
			n++
		case strings.HasPrefix(raw, "-"):
		default:
			n++
		}
	}
	return out
}

// redact keeps a reported line recognisable without repeating the secret in
// chat, which would leak it a second time.
func redact(s string) string {
	s = truncate(s, 140)
	var b strings.Builder
	run := 0
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			run++
			if run > 6 {
				b.WriteRune('•')
				continue
			}
		} else {
			run = 0
		}
		b.WriteRune(r)
	}
	return b.String()
}
