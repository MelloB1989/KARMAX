package runtime

import (
	"context"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/MelloB1989/karmax/internal/memory"
	"github.com/MelloB1989/karmax/internal/social"
	"github.com/MelloB1989/karmax/internal/store"
	"go.uber.org/zap"
)

// Who a public post may not name.
//
// The posts this feeds go out with nobody reading them first, so "never name a
// client" has to be a list rather than an instruction. The list is built from
// what KARMAX already knows about this particular life — the address book, and
// the subjects its memory is filed under — because a generic list of names
// could not contain the name of somebody's client.
//
// Memory paths are the good source here: GitLoom files every memory about one
// subject at one path, so facts/people/<name>.md and facts/projects/<name>.md
// ARE the names, already extracted by the thing that has been reading this
// person's life for months.

// forbiddenNames caches the list, since it is consulted on every post and
// changes about as often as somebody makes a new friend.
type forbiddenNames struct {
	db  *store.Store
	log *zap.Logger

	mu      sync.Mutex
	factory *memory.ManagerFactory
	names   []string
	builtAt time.Time
}

// forbiddenTTL is how long the list is reused. Long enough to cost nothing,
// short enough that a contact added today is protected today.
const forbiddenTTL = 30 * time.Minute

func newForbiddenNames(db *store.Store, log *zap.Logger) *forbiddenNames {
	return &forbiddenNames{db: db, log: log}
}

// attach supplies memory once the factory exists.
//
// The connectors are registered before memory is built, and they only ever call
// List lazily — at post time, long after startup — so the ordering costs
// nothing. Until it is attached the list is contacts only.
func (f *forbiddenNames) attach(factory *memory.ManagerFactory) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.factory, f.names, f.builtAt = factory, nil, time.Time{}
}

// List returns the names a post must not contain.
//
// A failure to build it is logged rather than swallowed, because an empty list
// is not a neutral outcome: the guard falls back to its pattern rules (money,
// phone numbers, credentials) and stops catching names entirely.
func (f *forbiddenNames) List() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.names != nil && time.Since(f.builtAt) < forbiddenTTL {
		return f.names
	}

	seen := map[string]bool{}
	var out []string
	add := func(name string) {
		name = strings.TrimSpace(name)
		if len([]rune(name)) < 4 || seen[strings.ToLower(name)] {
			return
		}
		seen[strings.ToLower(name)] = true
		out = append(out, name)
	}

	// Names the operator listed themselves.
	//
	// The two automatic sources are good but not complete: they know who is in
	// the address book and what memory files things under, and somebody who is
	// neither — mentioned only inside the text of a fact — is invisible to both.
	// This is how that gap gets closed, and it is worth telling the operator
	// about rather than pretending the automatic list is exhaustive.
	for _, n := range splitCSV(os.Getenv("KARMAX_SOCIAL_FORBIDDEN")) {
		add(n)
	}

	if names, err := f.db.ContactNames(); err != nil {
		f.log.Warn("forbidden names: contacts unavailable, posts will not be checked against them",
			zap.Error(err))
	} else {
		for _, n := range names {
			add(n)
		}
	}

	if f.factory != nil {
		for _, m := range f.factory.Managers() {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			surveyed, err := m.Survey(ctx, 0)
			cancel()
			if err != nil {
				f.log.Warn("forbidden names: memory unavailable, posts will not be checked against its subjects",
					zap.Error(err))
				continue
			}
			for _, e := range surveyed {
				if subject := subjectOf(e.ID); subject != "" {
					add(subject)
				}
			}
		}
	}

	f.names, f.builtAt = out, time.Now()
	f.log.Info("forbidden names rebuilt", zap.Int("count", len(out)))
	return out
}

// subjectOf turns a memory path into the subject it is about, or "" when the
// path is not about a subject at all.
//
// facts/projects/campx.md → "campx"; facts/context/newtra-ev.md → "newtra ev".
// The hyphens become spaces so the guard's per-word matching catches "Newtra"
// on its own, which is how somebody would actually write it.
//
// The empty case is the important one. Memory does not only file things under
// names — it also files them under whole sentences, which arrive here as paths
// like facts/context/deployment-check-on-2026-08-10-karmax-moved.md. Treating
// that as a name puts "spent", "three" and "long" on the forbidden list, and
// then an honest post about a race condition is refused for containing the word
// "three". Those are dropped: a subject is a short phrase, not a sentence, and
// it does not have a date in it.
func subjectOf(path string) string {
	name := path
	if i := strings.LastIndexByte(name, '/'); i >= 0 {
		name = name[i+1:]
	}
	name = strings.TrimSuffix(name, ".md")
	if i := strings.IndexByte(name, '#'); i >= 0 {
		name = name[:i]
	}
	name = strings.ReplaceAll(name, "-", " ")

	words := strings.Fields(name)
	if len(words) == 0 || len(words) > 3 {
		return ""
	}
	for _, w := range words {
		if strings.ContainsAny(w, "0123456789") {
			return "" // a date, so a sentence
		}
	}
	// A single common word ("funding", "college") names nobody, and forbidding
	// it costs a whole topic the operator can no longer post about.
	if len(words) == 1 && social.Generic(words[0]) {
		return ""
	}
	return name
}

// socialRecorder adapts the store to what the limiter needs.
type socialRecorder struct{ db *store.Store }

func (r socialRecorder) RecordPost(platform, status, postID, text, detail string) error {
	return r.db.RecordPost(store.SocialPost{
		Platform: platform, Status: status, PostID: postID, Text: text, Detail: detail,
	})
}

func (r socialRecorder) CountPostsSince(platform string, since time.Time) (int, error) {
	return r.db.CountPostsSince(platform, since)
}

func (r socialRecorder) LastPostAt(platform string) (time.Time, error) {
	return r.db.LastPostAt(platform)
}

// socialKillSwitch is the group and key the operator's off switch lives at.
const (
	socialKillGroup = "social"
	socialKillKey   = "posting_off"
)

// newSocialLimiter builds the rate limit and the off switch.
//
// The switch is checked at post time from two places, and either one stops it:
// the environment, so a systemd drop-in can disable posting on a machine, and
// the store, so `karmax social off` works on a running daemon without a
// restart. Two sources rather than one because the moment somebody wants this
// off is the moment they do not want to be reading documentation.
func newSocialLimiter(db *store.Store) *social.Limiter {
	perDay, minGap := social.DefaultPerDay, social.DefaultMinGap
	if n, err := strconv.Atoi(os.Getenv("KARMAX_SOCIAL_PER_DAY")); err == nil && n > 0 {
		perDay = n
	}
	if d, err := time.ParseDuration(os.Getenv("KARMAX_SOCIAL_MIN_GAP")); err == nil && d > 0 {
		minGap = d
	}
	return &social.Limiter{
		Rec:    socialRecorder{db: db},
		PerDay: perDay,
		MinGap: minGap,
		Disabled: func() (bool, string) {
			if v := strings.ToLower(os.Getenv("KARMAX_SOCIAL_POSTING")); v == "off" || v == "false" || v == "0" {
				return true, "posting is switched off by KARMAX_SOCIAL_POSTING"
			}
			if v, found, err := db.KVGet(socialKillGroup, socialKillKey); err == nil && found && v != "" {
				return true, "posting is switched off (" + v + ") — turn it back on with `karmax social on`"
			}
			return false, ""
		},
	}
}
