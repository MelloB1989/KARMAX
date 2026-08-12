package tools

import (
	"sort"
	"strings"
)

// A one-line-per-tool catalogue, for tools the model is told about but not given.
//
// Seventy-one full JSON schemas cost roughly 5,000 tokens in every request,
// carried into a decision that is usually "act or delegate". An index costs a
// line each: enough for the model to know a capability exists and ask for it,
// and the schema arrives on the turn it is actually needed.

// Index renders manifests as a catalogue, one line per tool, sorted so the text
// is byte-identical between turns — a prefix that reorders itself cannot be
// cached.
func Index(manifests []ToolManifest) string {
	if len(manifests) == 0 {
		return ""
	}
	sorted := make([]ToolManifest, len(manifests))
	copy(sorted, manifests)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })

	var b strings.Builder
	for _, m := range sorted {
		b.WriteString("- ")
		b.WriteString(m.Name)
		if s := Summarize(m.Description); s != "" {
			b.WriteString(": ")
			b.WriteString(s)
		}
		b.WriteByte('\n')
	}
	return b.String()
}

// Summarize reduces a tool description to its first sentence, bounded.
//
// Descriptions run to ten lines with worked examples — useful when the model is
// about to call the tool, noise when it is only deciding whether to ask for it.
func Summarize(desc string) string {
	desc = strings.TrimSpace(desc)
	if desc == "" {
		return ""
	}
	// Collapse to one line first: a description's own newlines would otherwise
	// break the one-tool-per-line shape the index depends on.
	desc = strings.Join(strings.Fields(desc), " ")

	if i := sentenceEnd(desc); i > 0 {
		desc = desc[:i]
	}
	const maxSummary = 140
	if len(desc) > maxSummary {
		if cut := strings.LastIndexByte(desc[:maxSummary], ' '); cut > 40 {
			return desc[:cut] + "…"
		}
		return desc[:maxSummary] + "…"
	}
	return desc
}

// sentenceEnd finds the first sentence terminator, ignoring the dots inside
// tool names like "comms.send" and abbreviations like "e.g.".
func sentenceEnd(s string) int {
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '.':
			// A dot between two letters is part of a name, not a full stop.
			if i+1 < len(s) && s[i+1] != ' ' {
				continue
			}
			if i > 0 && isAbbrevTail(s[:i+1]) {
				continue
			}
			return i + 1
		case '!', '?':
			if i+1 >= len(s) || s[i+1] == ' ' {
				return i + 1
			}
		}
	}
	return -1
}

func isAbbrevTail(s string) bool {
	for _, a := range []string{"e.g.", "i.e.", "etc.", "vs.", "cf."} {
		if strings.HasSuffix(strings.ToLower(s), a) {
			return true
		}
	}
	return false
}
