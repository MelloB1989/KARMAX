package social

import (
	_ "embed"
	"strings"
	"sync"
)

// Ordinary English, so a memory subject can be told apart from a name.
//
// The forbidden list is built partly from what memory files things under, and
// memory files things under topics as readily as under names:
// facts/context/campx.md sits beside facts/context/automation.md. Treating both
// as names made the words "automation", "alerts", "personal" and "project"
// unpublishable, and a hand-written list of exceptions never caught up — every
// one of them is just a common English word, and there are thousands more.
//
// A dictionary settles it in one test. "automation" is in it; "campx",
// "srikanth" and "newtra" are not, which is precisely what makes them names.
//
// Only lowercase entries are included: a proper noun that appears in a
// dictionary is exactly the kind of word this should still catch.
//
//go:embed english.txt
var englishWords string

var (
	englishOnce sync.Once
	english     map[string]bool
)

// Ordinary reports whether a word is ordinary English rather than a name.
//
// Applied to memory subjects, not to contact names. Somebody saved in an
// address book as "Rose" or "Mark" is a person the operator knows, and a
// dictionary would talk us out of protecting them.
func Ordinary(word string) bool {
	englishOnce.Do(func() {
		english = make(map[string]bool, 64<<10)
		for _, w := range strings.Split(englishWords, "\n") {
			if w != "" {
				english[w] = true
			}
		}
	})
	return english[strings.ToLower(strings.TrimSpace(word))]
}

// Topic reports whether a word names a subject rather than a person.
//
// The dictionary plus the short built-in list, because a dictionary of ordinary
// English does not contain "runtime", "workflow" or "gateway" — words that are
// unmistakably topics to anybody who writes software and unknown to a 1990s
// spellchecker.
func Topic(word string) bool { return Ordinary(word) || Generic(word) }
