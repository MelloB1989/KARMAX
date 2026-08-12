package memory

import "testing"

const sampleProfile = `<!-- last updated: 2026-08-13T00:00:00Z -->

# Kartik Deshmukh — Full Profile

## Identity
Founder. Builds KARMAX.

## Active Projects

### GitLoom
A git-backed memory engine.

### CampX × TrustStrike (VAPT/Security Audit)
Six reports for two lakh.

## ⚠️ Most Time-Sensitive Right Now
Chase Siva for the APK.
`

func TestProfileSplitsOnHeadings(t *testing.T) {
	got := splitProfileSections(sampleProfile)
	if len(got) != 6 {
		t.Fatalf("expected 6 sections, got %d: %+v", len(got), got)
	}
	if got[1].Title != "Identity" || got[1].Level != 2 {
		t.Errorf("second section = %q level %d", got[1].Title, got[1].Level)
	}
	if got[1].Body != "Founder. Builds KARMAX." {
		t.Errorf("body = %q", got[1].Body)
	}
	// A nested heading starts its own section rather than being swallowed.
	if got[3].Title != "GitLoom" || got[3].Level != 3 {
		t.Errorf("nested heading not split: %q level %d", got[3].Title, got[3].Level)
	}
}

// The agent should not have to reproduce emoji or punctuation to ask for a
// section — that is a retrieval failure disguised as a missing memory.
func TestSectionLookupToleratesMessyTitles(t *testing.T) {
	sections := splitProfileSections(sampleProfile)
	find := func(name string) string {
		want := normalizeHeading(name)
		for _, s := range sections {
			if normalizeHeading(s.Title) == want {
				return s.Title
			}
		}
		for _, s := range sections {
			if len(normalizeHeading(s.Title)) >= len(want) && normalizeHeading(s.Title)[:len(want)] == want {
				return s.Title
			}
		}
		return ""
	}
	for _, tc := range []struct{ query, want string }{
		{"Identity", "Identity"},
		{"identity", "Identity"},
		{"most time sensitive", "⚠️ Most Time-Sensitive Right Now"},
		{"campx", "CampX × TrustStrike (VAPT/Security Audit)"},
	} {
		if got := find(tc.query); got != tc.want {
			t.Errorf("find(%q) = %q, want %q", tc.query, got, tc.want)
		}
	}
}

// "#hashtag" and "#!/bin/sh" are not headings.
func TestNonHeadingsAreNotSplitPoints(t *testing.T) {
	got := splitProfileSections("## Real\ntext\n#nothashtag\n####### too many\nmore\n")
	if len(got) != 1 {
		t.Fatalf("expected 1 section, got %d: %+v", len(got), got)
	}
	if got[0].Title != "Real" {
		t.Errorf("title = %q", got[0].Title)
	}
}
