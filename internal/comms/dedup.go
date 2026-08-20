package comms

import (
	"errors"
	"strings"
	"sync"
	"time"
	"unicode"
)

// ErrDuplicateSend refuses a message that has just been sent to the same
// person, so a caller can tell "already said" apart from a real failure.
var ErrDuplicateSend = errors.New("comms: refusing to repeat a message already sent to this target")

// Every outbound message in KARMAX crosses Manager.send, and until this existed
// nothing there noticed it had said the same thing moments ago. wa-monitor
// carried its own suppression, which protected only wa-monitor: a reply drafted
// by the sweep, a comms.send from the orchestrator and a recipe could each send
// to one chat without any of them seeing the others.
//
// Naureen's thread is what that looks like from the outside — six messages, no
// replies, in three pairs sent 16, 31 and 30 seconds apart. Two pairs identical
// to the character, the third reworded, which is the tell: the same intent
// reached the send path twice and only the wording differed.
const (
	// identicalWindow: the same words to the same person inside a working day
	// is a bug, not a follow-up.
	identicalWindow = 6 * time.Hour
	// rephraseWindow is deliberately short. A reworded nudge a day later is
	// legitimate; the same thought twice in ten minutes is the duplicate.
	rephraseWindow = 10 * time.Minute
	// rephraseRatio is how much of the shorter message's meaningful words must
	// appear in the other to count as the same message said differently.
	rephraseRatio = 0.6
	// minWordsForRephrase keeps short acks ("ok", "done, thanks") out of the
	// similarity test, where any two of them look alike.
	minWordsForRephrase = 6
)

// sendGuard holds messages a send is in flight for, so two callers racing to
// say the same thing cannot both pass the check before either has landed in
// the store. The store check alone closes gaps of seconds, not milliseconds.
type sendGuard struct {
	mu       sync.Mutex
	inflight map[string]time.Time
}

func newSendGuard() *sendGuard {
	return &sendGuard{inflight: make(map[string]time.Time)}
}

// reserve claims (target, content) if nothing else holds it. release frees it.
func (g *sendGuard) reserve(key string, now time.Time) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	for k, at := range g.inflight {
		if now.Sub(at) > time.Minute {
			delete(g.inflight, k)
		}
	}
	if _, held := g.inflight[key]; held {
		return false
	}
	g.inflight[key] = now
	return true
}

func (g *sendGuard) release(key string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.inflight, key)
}

// isRepeat reports whether content has just been said to target, and why.
//
// recent is the outbound history for that target, newest first.
func isRepeat(content string, recent []pastMessage, now time.Time) (string, bool) {
	norm := normalizeMessage(content)
	if norm == "" {
		return "", false
	}
	words := significantWords(norm)

	for _, p := range recent {
		age := now.Sub(p.At)
		if age < 0 || age > identicalWindow {
			continue
		}
		prev := normalizeMessage(p.Content)
		if prev == "" {
			continue
		}
		if prev == norm {
			return "an identical message went to this target " + humanAge(age) + " ago", true
		}
		if age <= rephraseWindow && len(words) >= minWordsForRephrase {
			if overlapRatio(words, significantWords(prev)) >= rephraseRatio {
				return "the same message in different words went to this target " + humanAge(age) + " ago", true
			}
		}
	}
	return "", false
}

// pastMessage is one already-sent message, kept free of the store's types so
// the decision can be tested without a database.
type pastMessage struct {
	Content string
	At      time.Time
}

// normalizeMessage strips what varies between two sends of the same message:
// case, punctuation, emoji and spacing.
func normalizeMessage(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			b.WriteRune(r)
		default:
			b.WriteByte(' ')
		}
	}
	return strings.Join(strings.Fields(b.String()), " ")
}

func significantWords(norm string) map[string]bool {
	out := map[string]bool{}
	for _, w := range strings.Fields(norm) {
		if len(w) >= 4 && !commonWords[w] {
			out[w] = true
		}
	}
	return out
}

// overlapRatio is measured against the SHORTER message. A long message that
// happens to contain a short one says the short one again.
func overlapRatio(a, b map[string]bool) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	shared := 0
	for w := range a {
		if b[w] {
			shared++
		}
	}
	smaller := len(a)
	if len(b) < smaller {
		smaller = len(b)
	}
	return float64(shared) / float64(smaller)
}

func humanAge(d time.Duration) string {
	switch {
	case d < time.Minute:
		return d.Round(time.Second).String()
	case d < time.Hour:
		return d.Round(time.Minute).String()
	default:
		return d.Round(time.Minute).String()
	}
}

// commonWords are the words two unrelated messages share anyway.
var commonWords = map[string]bool{
	"just": true, "know": true, "that": true, "this": true, "with": true,
	"from": true, "have": true, "your": true, "will": true, "been": true,
	"they": true, "them": true, "there": true, "here": true, "what": true,
	"when": true, "then": true, "than": true, "also": true, "into": true,
	"about": true, "would": true, "could": true, "should": true, "please": true,
	"thanks": true, "hi": true, "hey": true, "hello": true,
}
