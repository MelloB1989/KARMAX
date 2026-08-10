package safety

import (
	"fmt"
	"regexp"
	"strings"
)

// Severity says what to do about a match. The asymmetry is deliberate: a web
// page or a message that mentions something alarming is worth flagging in
// context, but persisting it to memory is worth refusing, because recalled
// memory is later read as trusted.
type Severity int

const (
	Warn Severity = iota
	Block
)

func (s Severity) String() string {
	if s == Block {
		return "block"
	}
	return "warn"
}

// Threat is one matched attack pattern.
type Threat struct {
	Class    string
	Severity Severity
	Match    string
}

type rule struct {
	class    string
	severity Severity
	re       *regexp.Regexp
}

// The blocking rules are imperative constructions — text trying to change the
// model's instructions. Merely mentioning a secret path is a warning, because
// people legitimately write "the key lives in .env" and a memory system that
// refuses that is one people work around.
var rules = []rule{
	{"instruction-override", Block, regexp.MustCompile(`(?i)\b(ignore|disregard|forget)\b[^.\n]{0,40}\b(all\s+)?(previous|prior|earlier|above|your)\b[^.\n]{0,20}\b(instruction|prompt|rule|direction)`)},
	{"role-hijack", Block, regexp.MustCompile(`(?i)\b(you are now|from now on you are|new system prompt|act as (the )?(system|admin|developer)|pretend to be)\b`)},
	{"prompt-extraction", Block, regexp.MustCompile(`(?i)\b(reveal|print|show|repeat|output)\b[^.\n]{0,30}\b(system prompt|your instructions|initial prompt)\b`)},
	// The gap stays dot-free so it cannot run across a sentence; ".env" supplies
	// its own leading dot, which a \b before it would not match.
	{"exfiltration", Block, regexp.MustCompile(`(?i)\b(send|post|upload|forward|exfiltrate|email|leak)\b[^.\n]{0,40}(\.env\b|\b(api[ _-]?keys?|secrets?|tokens?|credentials?|passwords?|private keys?)\b)`)},
	{"tool-coercion", Block, regexp.MustCompile(`(?i)\b(run|execute|invoke|call)\b[^.\n]{0,30}\b(wacli|systemctl|curl|bash|sh|rm)\b`)},

	{"secret-path", Warn, regexp.MustCompile(`(?i)(~/\.karmax|\.env\b|id_rsa|\.ssh/|credentials\.json|service[_-]account)`)},
	{"destructive-command", Warn, regexp.MustCompile(`(?i)(rm\s+-rf|drop\s+table|truncate\s+table|mkfs|dd\s+if=)`)},
	{"persistence", Warn, regexp.MustCompile(`(?i)(crontab\s+-|systemctl\s+(--user\s+)?(enable|restart)|\.bashrc|\.zshrc|authorized_keys)`)},
}

// Scan returns the attack patterns present in text.
func Scan(text string) []Threat {
	if strings.TrimSpace(text) == "" {
		return nil
	}
	var out []Threat
	for _, r := range rules {
		if m := r.re.FindString(text); m != "" {
			out = append(out, Threat{Class: r.class, Severity: r.severity, Match: truncate(m, 120)})
		}
	}
	return out
}

// Blocking reports the subset that should stop a write.
func Blocking(threats []Threat) []Threat {
	var out []Threat
	for _, t := range threats {
		if t.Severity == Block {
			out = append(out, t)
		}
	}
	return out
}

// CheckWrite refuses text that should not be persisted as memory.
//
// Memory poisoning is the attack this exists for: a crafted message gets
// remembered, then recalled later as context the model trusts. The user can
// intervene at a write, which is why this blocks rather than warns.
func CheckWrite(text string) error {
	blocking := Blocking(Scan(text))
	if len(blocking) == 0 {
		return nil
	}
	classes := make([]string, 0, len(blocking))
	for _, t := range blocking {
		classes = append(classes, fmt.Sprintf("%s (%q)", t.Class, t.Match))
	}
	return fmt.Errorf("safety: refusing to store text that looks like a prompt-injection attempt: %s",
		strings.Join(classes, ", "))
}

// Describe renders threats for a log line or an operator alert.
func Describe(threats []Threat) string {
	if len(threats) == 0 {
		return ""
	}
	parts := make([]string, 0, len(threats))
	for _, t := range threats {
		parts = append(parts, t.Severity.String()+":"+t.Class)
	}
	return strings.Join(parts, ", ")
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}
