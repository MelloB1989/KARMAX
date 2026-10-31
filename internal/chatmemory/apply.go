package chatmemory

import (
	"context"
	"regexp"
	"strings"
	"time"

	"github.com/MelloB1989/karmax/internal/memory"
	"go.uber.org/zap"
)

// apply carries out what the model proposed, refusing anything it may not do:
// touch a memory it was not shown, forget a whole document to retire one fact
// in it, or store a secret.
func (s *Syncer) apply(ctx context.Context, ops []op, shown []memory.SearchResult, at time.Time, res *Result) {
	handle := func(n int) (string, bool) {
		if n < 1 || n > len(shown) {
			return "", false
		}
		return shown[n-1].Entry.ID, true
	}
	for _, o := range ops {
		switch strings.ToLower(strings.TrimSpace(o.Op)) {
		case "add":
			e, ok := s.entry(o, at)
			if !ok {
				res.Refused++
				continue
			}
			if err := s.mem.Write(e); err != nil {
				s.log.Warn(s.cfg.Name+": could not add a fact", zap.String("fact", oneLine(e.Content, 120)), zapErr(err))
				res.Refused++
				continue
			}
			res.Added++

		case "update":
			h, ok := handle(o.Memory)
			e, ok2 := s.entry(o, at)
			if !ok || !ok2 {
				res.Refused++
				continue
			}
			// The correction first: a fact retired before its replacement is
			// stored is a fact lost when the write fails.
			if err := s.mem.Write(e); err != nil {
				s.log.Warn(s.cfg.Name+": could not write a correction", zap.String("memory", h), zapErr(err))
				res.Refused++
				continue
			}
			res.Updated++
			if removed, err := s.mem.ForgetFact(ctx, h); err != nil || !removed {
				// Kept beside its correction, which is newer and so wins.
				s.log.Info(s.cfg.Name+": corrected fact kept beside its correction", zap.String("memory", h), zapErr(err))
			}

		case "forget":
			h, ok := handle(o.Memory)
			if !ok {
				res.Refused++
				continue
			}
			removed, err := s.mem.ForgetFact(ctx, h)
			if err != nil || !removed {
				s.log.Info(s.cfg.Name+": forget refused", zap.String("memory", h), zapErr(err))
				res.Refused++
				continue
			}
			res.Forgotten++

		default:
			res.Refused++
		}
	}
}

// entry is the memory an add or update writes, or false when it must not be
// written at all.
func (s *Syncer) entry(o op, at time.Time) (memory.MemoryEntry, bool) {
	fact := strings.TrimSpace(o.Fact)
	subject := strings.ToLower(strings.TrimSpace(o.Subject))
	if fact == "" || subject == "" || looksSecret(fact) {
		return memory.MemoryEntry{}, false
	}
	when := at
	if d, err := time.ParseInLocation("2006-01-02", strings.TrimSpace(o.When), s.cfg.Location); err == nil {
		when = d
	}
	return memory.MemoryEntry{
		Role:    s.cfg.Tag,
		Content: fact,
		// The subject first: memory files a fact under its first tag.
		Tags:       []string{subject, s.cfg.Tag},
		Category:   category(o.Category),
		Importance: importance(o.Importance),
		OccurredAt: when,
	}, true
}

// category maps the model's word onto the categories memory files by.
func category(c string) string {
	switch strings.ToLower(strings.TrimSpace(c)) {
	case "relationship", "person", "people", "contact":
		return "relationship"
	case "project", "projects", "work":
		return "project"
	case "decision", "decisions":
		return "decision"
	case "preference", "preferences":
		return "preference"
	case "reference":
		return "reference"
	case "task", "tasks", "commitment", "deadline":
		return "task"
	}
	return "context"
}

func importance(s string) int {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "high":
		return 3
	case "critical":
		return 4
	case "low":
		return 1
	}
	return 2
}

var secretRes = []*regexp.Regexp{
	// A secret named and then given: "password is …", "OTP was 482913", "API key: …".
	regexp.MustCompile(`(?i)\b(password|passwd|passcode|otp|one[- ]time (code|password)|verification code|` +
		`api[ _-]?key|secret( key)?|access token|auth token|token|private key|cvv|upi pin|atm pin)\b` +
		`[^.\n]{0,30}?\b(is|was|are|:|=)\s*["'` + "`" + `]?[^\s"'` + "`" + `]{4,}`),
	// Well-known key shapes.
	regexp.MustCompile(`\b(sk-[A-Za-z0-9_-]{12,}|ghp_[A-Za-z0-9]{20,}|gho_[A-Za-z0-9]{20,}|AKIA[0-9A-Z]{16}|xox[abp]-[A-Za-z0-9-]{10,}|rpa_[A-Za-z0-9]{12,})`),
	// A long run of letters and digits together is a key, not a word.
	regexp.MustCompile(`\b[A-Za-z0-9_\-]*(?:[A-Za-z][0-9]|[0-9][A-Za-z])[A-Za-z0-9_\-]{30,}\b`),
}

// looksSecret reports text that would store a credential. Memory is recalled
// into prompts, loops and other agents; a secret in it is a secret handed to
// all of them.
func looksSecret(text string) bool {
	for _, re := range secretRes {
		if re.MatchString(text) {
			return true
		}
	}
	return false
}

func zapChat(c Chat) zap.Field { return zap.String("chat", key(c)) }

func zapErr(err error) zap.Field {
	if err == nil {
		return zap.Skip()
	}
	return zap.Error(err)
}
