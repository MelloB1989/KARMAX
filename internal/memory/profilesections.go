package memory

import (
	"fmt"
	"strings"
)

// Section-addressable reads over ABOUT_ME.md.
//
// The profile is the agent's understanding of the operator, and it grows: at the
// time this was written it was 21KB, and every turn carried the whole of it into
// a decision that only ever needed a line or two. Reading it in named pieces is
// what lets the always-on context hold a summary instead of the document.

// ProfileSection is one `##`/`###` block of the profile.
type ProfileSection struct {
	Title string // heading text, without the leading hashes
	Level int    // 1 for #, 2 for ##, 3 for ###
	Body  string // content beneath the heading, excluding nested deeper headings
	Chars int
}

// ProfileSections splits the profile on its Markdown headings, in file order.
func (m *Manager) ProfileSections() ([]ProfileSection, error) {
	doc, err := m.ReadProfile()
	if err != nil {
		return nil, err
	}
	return splitProfileSections(doc), nil
}

// ProfileSectionNamed returns one section, matched case-insensitively on a
// prefix of the title so the agent does not have to reproduce punctuation and
// emoji exactly.
func (m *Manager) ProfileSectionNamed(name string) (ProfileSection, error) {
	sections, err := m.ProfileSections()
	if err != nil {
		return ProfileSection{}, err
	}
	want := normalizeHeading(name)
	if want == "" {
		return ProfileSection{}, fmt.Errorf("no section name given")
	}
	for _, s := range sections {
		if normalizeHeading(s.Title) == want {
			return s, nil
		}
	}
	for _, s := range sections {
		if strings.HasPrefix(normalizeHeading(s.Title), want) {
			return s, nil
		}
	}
	for _, s := range sections {
		if strings.Contains(normalizeHeading(s.Title), want) {
			return s, nil
		}
	}
	return ProfileSection{}, fmt.Errorf("no section matching %q", name)
}

func splitProfileSections(doc string) []ProfileSection {
	var out []ProfileSection
	var cur *ProfileSection
	var body strings.Builder

	flush := func() {
		if cur == nil {
			return
		}
		cur.Body = strings.TrimSpace(body.String())
		cur.Chars = len(cur.Body)
		out = append(out, *cur)
		body.Reset()
	}

	for _, line := range strings.Split(doc, "\n") {
		if level, title, ok := parseHeading(line); ok {
			flush()
			cur = &ProfileSection{Title: title, Level: level}
			continue
		}
		if cur != nil {
			body.WriteString(line)
			body.WriteByte('\n')
		}
	}
	flush()
	return out
}

func parseHeading(line string) (level int, title string, ok bool) {
	trimmed := strings.TrimLeft(line, " ")
	if !strings.HasPrefix(trimmed, "#") {
		return 0, "", false
	}
	i := 0
	for i < len(trimmed) && trimmed[i] == '#' {
		i++
	}
	// "#hashtag" is not a heading; a heading has whitespace after the hashes.
	if i > 6 || i >= len(trimmed) || (trimmed[i] != ' ' && trimmed[i] != '\t') {
		return 0, "", false
	}
	return i, strings.TrimSpace(trimmed[i:]), true
}

// normalizeHeading lowercases and drops everything that is not a letter, digit
// or space, so "⚠️ Most Time-Sensitive Right Now" is reachable as
// "most time sensitive".
func normalizeHeading(s string) string {
	var b strings.Builder
	lastSpace := false
	for _, r := range strings.ToLower(strings.TrimSpace(s)) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			lastSpace = false
		default:
			if !lastSpace && b.Len() > 0 {
				b.WriteByte(' ')
				lastSpace = true
			}
		}
	}
	return strings.TrimSpace(b.String())
}
