package runtime

import (
	"regexp"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"
)

// Text on its way to a synthesiser: split into sentences as the model streams,
// and normalised so what is spoken is what a person would say.

const (
	// minSpokenRunes merges fragments shorter than this into the next sentence.
	minSpokenRunes = 16
	// softClauseRunes is how long a sentence may run before a comma may end a chunk.
	softClauseRunes = 80
)

// spokenAbbreviations end in a dot without ending the sentence.
var spokenAbbreviations = map[string]bool{
	"mr": true, "mrs": true, "ms": true, "dr": true, "prof": true, "sr": true, "jr": true,
	"st": true, "vs": true, "inc": true, "ltd": true, "co": true, "fig": true, "vol": true,
	"approx": true, "dept": true, "est": true, "mt": true,
}

var dottedInitialism = regexp.MustCompile(`^(?:[a-z]\.)+[a-z]$`)

// sentenceSplitter turns a stream of text deltas into speakable sentences.
type sentenceSplitter struct{ buf string }

func (s *sentenceSplitter) Push(delta string) []string {
	s.buf += delta
	return s.drain(false)
}

func (s *sentenceSplitter) Flush() []string { return s.drain(true) }

func (s *sentenceSplitter) drain(final bool) []string {
	var out []string
	for {
		end := s.boundary(final)
		if end <= 0 {
			break
		}
		if chunk := strings.TrimSpace(s.buf[:end]); chunk != "" {
			out = append(out, chunk)
		}
		s.buf = s.buf[end:]
	}
	if final {
		if rest := strings.TrimSpace(s.buf); rest != "" {
			out = append(out, rest)
		}
		s.buf = ""
	}
	return out
}

func isTerminator(r rune) bool { return strings.ContainsRune(".!?।॥…", r) }
func isCloser(r rune) bool     { return strings.ContainsRune("\"')]”’*_", r) }

// boundary is the byte offset where the first chunk of at least minSpokenRunes ends, or 0.
func (s *sentenceSplitter) boundary(final bool) int {
	b := s.buf
	runes := 0
	for i := 0; i < len(b); {
		r, w := utf8.DecodeRuneInString(b[i:])
		runes++
		switch {
		case r == '\n':
			if runes >= minSpokenRunes {
				return i + w
			}
		case isTerminator(r):
			j := i + w
			for j < len(b) {
				r2, w2 := utf8.DecodeRuneInString(b[j:])
				if !isTerminator(r2) && !isCloser(r2) {
					break
				}
				j += w2
			}
			runLen := utf8.RuneCountInString(b[i+w : j])
			danda := r == '।' || r == '॥'
			notEnd := false
			switch {
			case danda:
			case j >= len(b):
				if !final {
					return 0
				}
			default:
				next, _ := utf8.DecodeRuneInString(b[j:])
				notEnd = !unicode.IsSpace(next) || (r == '.' && abbreviationBefore(b[:i]))
			}
			if !notEnd && runes+runLen >= minSpokenRunes {
				return j
			}
			runes += runLen
			i = j
			continue
		case r == ',' || r == ';' || r == ':':
			if i+w < len(b) && b[i+w] == ' ' {
				if (r != ',' && runes >= minSpokenRunes) || (r == ',' && runes >= softClauseRunes) {
					return i + w
				}
			}
		}
		i += w
	}
	return 0
}

// abbreviationBefore reports whether the dot after text belongs to an abbreviation, initial or list number.
func abbreviationBefore(text string) bool {
	tok := text
	if k := strings.LastIndexAny(tok, " \t\n"); k >= 0 {
		tok = tok[k+1:]
	}
	tok = strings.ToLower(strings.TrimLeft(tok, "(\"'[*_"))
	if tok == "" {
		return false
	}
	if spokenAbbreviations[tok] || dottedInitialism.MatchString(tok) {
		return true
	}
	if utf8.RuneCountInString(tok) == 1 && unicode.IsLetter([]rune(tok)[0]) && tok != "i" && tok != "a" {
		return true
	}
	if len(tok) <= 2 && strings.Trim(tok, "0123456789") == "" {
		return true
	}
	return false
}

// turnSpeech streams one turn's text to the integration a sentence at a time.
type turnSpeech struct {
	tags    bool
	mu      sync.Mutex
	split   sentenceSplitter
	say     func(string) bool
	spoken  int
	stopped bool
}

func newTurnSpeech(say func(string) bool, tags bool) *turnSpeech {
	if say == nil {
		return nil
	}
	return &turnSpeech{say: say, tags: tags}
}

// Write takes a model delta.
func (t *turnSpeech) Write(delta string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.emit(t.split.Push(delta))
}

// Flush sends whatever is left at the end of the turn.
func (t *turnSpeech) Flush() {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.emit(t.split.Flush())
}

// Spoken is how many sentences were handed over.
func (t *turnSpeech) Spoken() int {
	if t == nil {
		return 0
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.spoken
}

func (t *turnSpeech) emit(sentences []string) {
	for _, s := range sentences {
		if t.stopped {
			return
		}
		s = speakableFor(s, t.tags)
		if !hasSpeech(s) {
			continue
		}
		if !t.say(s) {
			t.stopped = true
			return
		}
		t.spoken++
	}
}

func hasSpeech(s string) bool {
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return true
		}
	}
	return false
}

// speakable normalises text for a synthesiser. The agent writes for a screen
// everywhere else and its habits come with it: asterisks read as "asterisk", a
// bullet list becomes a monotone, "$20" and "&" are mangled.
func speakable(s string) string { return speakableFor(s, false) }

// audioTag is an inline performance cue such as [laughs].
var audioTag = regexp.MustCompile(`\[[A-Za-z][A-Za-z ,'-]{0,30}\]`)

// speakableFor is speakable for a voice that performs audio tags (tags true)
// or would read them aloud (false, so they are removed).
func speakableFor(s string, tags bool) string {
	s = markdownLink.ReplaceAllString(s, "$1")
	if !tags {
		return speakableText(audioTag.ReplaceAllString(s, " "))
	}
	var kept []string
	s = audioTag.ReplaceAllStringFunc(s, func(m string) string {
		if len(kept) >= 26 {
			return " "
		}
		kept = append(kept, strings.ToLower(m))
		return "\uE000" + string(rune('a'+len(kept)-1)) + "\uE001"
	})
	s = speakableText(s)
	for i, tag := range kept {
		s = strings.ReplaceAll(s, "\uE000"+string(rune('a'+i))+"\uE001", tag)
	}
	return s
}

func speakableText(s string) string {
	s = markdownLink.ReplaceAllString(s, "$1")
	// Identifiers are not speech. A JID, a LID, a phone number, a URL: read
	// aloud they are fifteen seconds of digits nobody wanted, and they arrive
	// because memory stores them next to the names.
	s = unspeakable.ReplaceAllString(s, "")
	// What is left of "his number is 9198…" once the digits go is "his
	// number is ." — drop the stump too, keeping the punctuation.
	s = danglingIdentifier.ReplaceAllString(s, "$1")

	s = codeFence.ReplaceAllString(s, " ")
	s = hashNumber.ReplaceAllString(s, "number $1")
	s = strings.NewReplacer("**", "", "~~", "", "*", "", "`", "", "#", "", "_", " ",
		"“", `"`, "”", `"`, "‘", "'", "’", "'", "[", "", "]", "").Replace(s)
	s = stripEmoji(s)

	s = currencyAmount.ReplaceAllStringFunc(s, spellCurrency)
	s = magnitude.ReplaceAllStringFunc(s, spellMagnitude)
	s = unitAfterNumber.ReplaceAllStringFunc(s, spellUnit)
	s = strings.NewReplacer(
		"e.g.", "for example,", "i.e.", "that is,", "w/o", "without", "w/", "with",
		"°C", " degrees Celsius", "°F", " degrees Fahrenheit", "°", " degrees",
		"->", " to ", "→", " to ", "=>", " to ", "≈", " about ", "≥", " at least ", "≤", " at most ",
	).Replace(s)
	s = numberRange.ReplaceAllString(s, "$1 to $2")
	s = approxNumber.ReplaceAllString(s, "about $1")
	s = percent.ReplaceAllString(s, "$1 percent")
	s = strings.NewReplacer("%", " percent", "&", " and ", "+", " plus ", "=", " equals ", "@", " at ").Replace(s)
	s = wordSlash.ReplaceAllString(s, "$1 $2")
	s = spacedDash.ReplaceAllString(s, ", ")
	s = strings.NewReplacer("—", ", ", "–", ", ", "…", "...").Replace(s)

	var out []string
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(line), "-•→>◦▪ "))
		line = strings.Join(strings.Fields(line), " ")
		if line == "" {
			continue
		}
		if n := len(out); n > 0 && !endsSentence(out[n-1]) {
			out[n-1] += "."
		}
		out = append(out, line)
	}
	s = strings.Join(out, " ")
	s = strings.Join(strings.Fields(s), " ")
	s = strings.NewReplacer(" ,", ",", " .", ".", " ?", "?", " !", "!").Replace(s)
	s = repeatedComma.ReplaceAllString(s, ",")
	s = commaBeforeStop.ReplaceAllString(s, "$1")
	return strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(s), ",;:"))
}

func endsSentence(s string) bool {
	r, _ := utf8.DecodeLastRuneInString(s)
	return strings.ContainsRune(".!?।॥,:;", r)
}

func stripEmoji(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r == 0xFE0F || r == 0x200D || r == 0x20E3:
			return -1
		case r >= 0x1F000 && r <= 0x1FAFF, r >= 0x2600 && r <= 0x27BF, r >= 0x2B00 && r <= 0x2BFF,
			r >= 0x2190 && r <= 0x21FF, r >= 0x2300 && r <= 0x23FF:
			return ' '
		}
		return r
	}, s)
}

var (
	markdownLink    = regexp.MustCompile(`\[([^\]]*)\]\([^)]*\)`)
	codeFence       = regexp.MustCompile("(?s)```.*?```")
	hashNumber      = regexp.MustCompile(`#(\d+)`)
	currencyAmount  = regexp.MustCompile(`([$₹€£])\s?(\d[\d,]*(?:\.\d+)?)(?:\s?(k|K|M|B|bn|lakh|crore)\b)?`)
	magnitude       = regexp.MustCompile(`\b(\d+(?:\.\d+)?)\s?([KMB])\b`)
	unitAfterNumber = regexp.MustCompile(`\b(\d+(?:\.\d+)?)\s?(GB|MB|KB|TB|ms|km|kg)\b`)
	numberRange     = regexp.MustCompile(`(\d)\s?[–-]\s?(\d)`)
	approxNumber    = regexp.MustCompile(`~\s?(\d)`)
	percent         = regexp.MustCompile(`(\d)\s?%`)
	wordSlash       = regexp.MustCompile(`([A-Za-z])/([A-Za-z])`)
	spacedDash      = regexp.MustCompile(`[ \t]+-+[ \t]+`)
	repeatedComma   = regexp.MustCompile(`,(?:\s*,)+`)
	commaBeforeStop = regexp.MustCompile(`,\s*([.!?।])`)
)

var currencyName = map[string]string{"$": "dollars", "₹": "rupees", "€": "euros", "£": "pounds"}

var magnitudeName = map[string]string{
	"k": "thousand", "K": "thousand", "M": "million", "B": "billion", "bn": "billion",
	"lakh": "lakh", "crore": "crore",
}

var unitName = map[string]string{
	"GB": "gigabytes", "MB": "megabytes", "KB": "kilobytes", "TB": "terabytes",
	"ms": "milliseconds", "km": "kilometres", "kg": "kilograms",
}

func spellCurrency(m string) string {
	p := currencyAmount.FindStringSubmatch(m)
	out := p[2] + " "
	if p[3] != "" {
		out += magnitudeName[p[3]] + " "
	}
	return out + currencyName[p[1]]
}

func spellMagnitude(m string) string {
	p := magnitude.FindStringSubmatch(m)
	return p[1] + " " + magnitudeName[p[2]]
}

func spellUnit(m string) string {
	p := unitAfterNumber.FindStringSubmatch(m)
	return p[1] + " " + unitName[p[2]]
}

// unspeakable matches things that must never be read aloud: WhatsApp ids,
// long digit runs (phone numbers, LIDs), and URLs.
var unspeakable = regexp.MustCompile(`(?i)\b\d[\d:]{7,}@[a-z.]+|\bhttps?://\S+|\b\d{9,}\b`)

var danglingIdentifier = regexp.MustCompile(
	`(?i)\s*[—–-]?\s*\b(?:his|her|their|the|whose)?\s*(?:number|id|jid|lid|phone|link|url)\s+(?:is|:)\s*([.,;!?]|$)`)
