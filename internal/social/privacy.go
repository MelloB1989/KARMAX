// Package social publishes to the operator's public accounts.
//
// Everything here exists because these posts go out WITHOUT a human reading
// them first. That is the operator's decision, and it makes this the only place
// in KARMAX where a model's output reaches strangers unreviewed — so the rules
// about what may be said are enforced in code rather than asked for in a
// prompt.
//
// A prompt is a request. A model that has been told "never name a client" will
// mostly comply, and the failures are exactly the interesting ones: the day
// something notable happened, with a name attached. Publishing that is not
// recoverable — a deleted post has already been seen, and the person it named
// did not choose to be written about.
package social

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// Guard decides whether a draft may be published.
type Guard struct {
	// Names the post must never contain: contacts, companies, projects. Supplied
	// by the caller from the operator's own address book and memory, because
	// KARMAX knows who is in this person's life and a generic list does not.
	Forbidden []string
	// MaxRunes bounds the post for the platform.
	MaxRunes int
	// Allowed are words that are never a name, whatever the address book or
	// memory says. The operator's escape hatch.
	//
	// Needed because the forbidden list is built from real data and real data
	// contains ordinary words: a contact called "Hyd T Service", a memory
	// subject called "automation". A built-in list of exceptions can only ever
	// contain the ones already seen, and each miss costs a refused post that
	// should have gone out.
	Allowed []string
}

// Refusal explains why a draft cannot be published.
type Refusal struct {
	// Reason is shown to the operator and given back to the model, so the next
	// attempt can avoid the same thing.
	Reason string
	// Found is what tripped it, for the log.
	Found []string
}

func (r *Refusal) Error() string {
	if len(r.Found) == 0 {
		return "social: " + r.Reason
	}
	return fmt.Sprintf("social: %s (%s)", r.Reason, strings.Join(r.Found, ", "))
}

// Patterns that are never publishable, whatever the prompt said.
//
// Money and contact details are here rather than in the forbidden-names list
// because they do not depend on knowing anybody: an amount or a phone number in
// a post about somebody's day is a leak by construction.
var (
	// An amount must START with a digit. It used to be [\d,]+, which a bare
	// comma satisfies — so "monitors, " parsed as "rs" followed by an amount,
	// and every ordinary post containing monitors, infers, developers or years
	// before a comma was refused for containing money.
	//
	// Money in the forms this operator's life actually contains: ₹2,00,000,
	// Rs 30000, $5k, 80k.
	moneyPattern = regexp.MustCompile(`(?i)(₹|rs\.?\s*|\$|usd\s*|inr\s*)\s?\d[\d,]*(\.\d+)?\s*(k|l|lakh|lakhs|cr|crore|m|million)?|\b\d[\d,]*\s*(k|l|lakh|lakhs|cr|crore)\b`)
	// "4k" is a screen, not a payment, and this operator writes about tech. A
	// guard that refuses every post mentioning a display is a guard that gets
	// switched off, which protects nothing.
	notMoney = regexp.MustCompile(`(?i)\b\d+\s*k\b[\s-]*(display|monitor|screen|video|res|resolution|footage|webcam|tv)`)
	// Phone numbers and WhatsApp JIDs.
	phonePattern = regexp.MustCompile(`(\+?\d[\d\s-]{8,}\d)|(\b\d{10,}@[a-z.]+)`)
	emailPattern = regexp.MustCompile(`[\w.+-]+@[\w-]+\.[\w.]+`)
	// Anything that looks like a credential. A post is never the place.
	secretPattern = regexp.MustCompile(`(?i)\b(sk-|xox[baprs]-|ghp_|gho_|github_pat_|ntn_|secret_|AIza|Bearer\s+[A-Za-z0-9._-]{20,})`)
	// An internal URL is somebody's private infrastructure.
	privateURLPattern = regexp.MustCompile(`(?i)https?://(localhost|127\.0\.0\.1|10\.|192\.168\.|172\.(1[6-9]|2\d|3[01])\.|[a-z0-9-]+\.local\b|[a-z0-9-]+\.internal\b)`)
)

// Check reports whether a draft may be published.
//
// Refusing is the default when anything is ambiguous. A post that does not go
// out costs nothing; the operator will see it was refused and why.
func (g Guard) Check(draft string) error {
	text := strings.TrimSpace(draft)
	if text == "" {
		return &Refusal{Reason: "the draft is empty"}
	}
	if g.MaxRunes > 0 && len([]rune(text)) > g.MaxRunes {
		return &Refusal{Reason: fmt.Sprintf("the draft is %d characters, over the %d limit",
			len([]rune(text)), g.MaxRunes)}
	}

	for _, c := range []struct {
		pattern *regexp.Regexp
		reason  string
	}{
		{secretPattern, "it contains something shaped like a credential"},
		{moneyPattern, "it contains an amount of money"},
		{phonePattern, "it contains a phone number or a WhatsApp id"},
		{emailPattern, "it contains an email address"},
		{privateURLPattern, "it contains a private or internal address"},
	} {
		found := c.pattern.FindAllString(text, 3)
		if c.pattern == moneyPattern {
			found = dropIf(found, notMoney.MatchString(text))
		}
		if len(found) > 0 {
			return &Refusal{Reason: c.reason, Found: found}
		}
	}

	if found := g.namesIn(text); len(found) > 0 {
		return &Refusal{
			Reason: "it names somebody or something from the operator's private life — " +
				"rewrite it without that and post again, keeping the substance",
			Found: found,
		}
	}
	return nil
}

// dropIf clears a match set when a narrower pattern explains it away.
func dropIf(found []string, explained bool) []string {
	if explained {
		return nil
	}
	return found
}

// namesIn finds forbidden names, matched on word boundaries.
//
// Whole words only: "Siva" must not be found inside "Sivakasi", and a
// two-letter contact name would otherwise match half the alphabet. Names
// shorter than four characters are skipped for the same reason.
//
// Case-insensitive, deliberately. An earlier version required a name to appear
// capitalised, on the theory that "Srikanth" is a name and "call" is not — but
// people write names in lower case all the time, and a guard that misses "long
// call with siva" is worse than one that occasionally refuses a post for saying
// "Funding". The list is kept clean at its source instead.
func (g Guard) namesIn(text string) []string {
	lower := strings.ToLower(text)
	seenName := map[string]bool{}
	seenHit := map[string]bool{}
	var found []string
	for _, name := range g.Forbidden {
		name = strings.ToLower(strings.TrimSpace(name))
		if len([]rune(name)) < 4 || seenName[name] {
			continue
		}
		seenName[name] = true
		for _, cand := range append([]string{name}, g.nameParts(name)...) {
			if !g.nameLike(cand) {
				continue
			}
			if !wordIn(lower, cand) || seenHit[cand] {
				continue
			}
			seenHit[cand] = true
			found = append(found, cand)
			break
		}
	}
	sort.Strings(found)
	return found
}

// nameParts are the individual words of a multi-word name.
//
// "CampX Technologies" must also catch a post that says only "CampX". The
// generic words are dropped because a real address book contains "Pavan Kumar
// Dishwash Service" and "Ram Call Whatsapp", and matching every word of those
// makes "service" and "call" unpublishable. A guard that refuses an honest post
// about a reporting service gets switched off, which protects nothing.
func (g Guard) nameParts(name string) []string {
	words := strings.Fields(name)
	if len(words) < 2 {
		return nil
	}
	var out []string
	for _, w := range words {
		if len([]rune(w)) >= 4 {
			out = append(out, w)
		}
	}
	return out
}

// nameLike reports whether a candidate can be somebody's name at all.
//
// It can if at least one of its words is not ordinary. This is what separates
// the real entries in an address book from the roles people also save there:
// "Agent" and "Property Agent" are jobs, "Agent Ram Uppal" is a person, and
// "Uppal" is the word that says so.
//
// The cost is a contact saved under a single ordinary word — somebody called
// "Rose" stops being protected, because nothing distinguishes that entry from
// the flower. KARMAX_SOCIAL_FORBIDDEN is the answer for those, and it is a
// better trade than the alternative: with "Agent" on the list, almost nothing
// anybody in software writes about their day can be published.
func (g Guard) nameLike(candidate string) bool {
	words := strings.Fields(candidate)
	if len(words) == 0 {
		return false
	}
	for _, w := range words {
		if !Topic(w) && !g.allows(w) {
			return true
		}
	}
	return false
}

// allows reports whether the operator declared this word never to be a name.
func (g Guard) allows(word string) bool {
	for _, a := range g.Allowed {
		if strings.EqualFold(strings.TrimSpace(a), word) {
			return true
		}
	}
	return false
}

// Generic reports whether a word names nobody — a common noun that turns up in
// real contact names and memory subjects. Exported because the caller building
// the forbidden list needs the same judgement, and two lists would drift.
func Generic(word string) bool { return generic[strings.ToLower(strings.TrimSpace(word))] }

// generic are words that turn up in real contact names and memory paths but
// name nobody.
var generic = map[string]bool{
	"service": true, "services": true, "customer": true, "care": true,
	"support": true, "office": true, "home": true, "work": true, "team": true,
	"call": true, "calls": true, "missed": true, "number": true, "phone": true,
	"group": true, "sales": true, "admin": true, "help": true, "desk": true,
	"centre": true, "center": true, "store": true, "shop": true, "repair": true,
	"driver": true, "doctor": true, "dentist": true, "bank": true, "school": true,
	"college": true, "hospital": true, "clinic": true, "hotel": true,
	"restaurant": true, "delivery": true, "courier": true, "electrician": true,
	"plumber": true, "carpenter": true, "cleaning": true, "laundry": true,
	"pharmacy": true, "medical": true, "travel": true, "tours": true,
	"agency": true, "solutions": true, "systems": true, "technologies": true,
	"private": true, "limited": true, "company": true, "corp": true,
	"whatsapp": true, "google": true, "apple": true, "amazon": true,
	"uber": true, "swiggy": true, "zomato": true, "gmail": true,
	"india": true, "main": true, "road": true, "friend": true, "boss": true,
	"brother": true, "sister": true, "uncle": true, "aunty": true,
	// Memory files things under plain topic names, and these are topics
	// somebody who builds software would obviously post about.
	"automation": true, "harness": true, "memory": true, "gateway": true,
	"sandbox": true, "pipeline": true, "runtime": true, "workflow": true,
	"agent": true, "agents": true, "model": true, "models": true,
	"security": true, "audit": true, "report": true, "reports": true,
	"alerts": true, "alert": true, "digest": true, "briefing": true,
	"deploy": true, "release": true, "backlog": true, "roadmap": true,
	// Memory files itself under plain topic names, and these are topics anybody
	// might post about.
	"funding": true, "drive": true, "events": true, "context": true,
	"deployment": true, "competitive": true, "tools": true, "notes": true,
	"general": true, "misc": true, "other": true, "status": true, "review": true,
}

// wordIn reports whether needle appears in haystack as a whole word.
func wordIn(haystack, needle string) bool {
	at := 0
	for {
		i := strings.Index(haystack[at:], needle)
		if i < 0 {
			return false
		}
		i += at
		before := i == 0 || !isWordByte(haystack[i-1])
		end := i + len(needle)
		after := end >= len(haystack) || !isWordByte(haystack[end])
		if before && after {
			return true
		}
		at = i + 1
		if at >= len(haystack) {
			return false
		}
	}
}

func isWordByte(b byte) bool {
	return b == '_' || (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9')
}
