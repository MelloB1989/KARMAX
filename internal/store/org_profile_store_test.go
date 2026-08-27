package store

import (
	"strings"
	"testing"
)

// An unconfigured org is a normal state, not an error every caller branches on.
func TestAnUnsetProfileReadsEmptyRatherThanFailing(t *testing.T) {
	s := newTestStore(t)

	p, err := s.OrgProfileFor(DefaultOrg)
	if err != nil {
		t.Fatalf("reading an unset profile failed: %v", err)
	}
	if p.Name != "" || p.Context != "" {
		t.Errorf("an unset profile came back populated: %+v", p)
	}
	// And it must render to nothing: a heading with nothing under it spends
	// context to tell the model the company has no name.
	if p.Briefing() != "" {
		t.Errorf("an empty profile produced a briefing: %q", p.Briefing())
	}
}

func TestTheProfileRoundTrips(t *testing.T) {
	s := newTestStore(t)

	if err := s.SaveOrgProfile(OrgProfile{
		Org: DefaultOrg, Name: "Zero Moblt", Domain: "zeromoblt.com",
		Description: "We build AI agents.", Timezone: "Asia/Kolkata",
		Context: "Tickets live in YouTrack, not Jira.", UpdatedBy: "nikhil",
	}); err != nil {
		t.Fatal(err)
	}

	p, err := s.OrgProfileFor(DefaultOrg)
	if err != nil {
		t.Fatal(err)
	}
	if p.Name != "Zero Moblt" || p.Timezone != "Asia/Kolkata" || p.UpdatedBy != "nikhil" {
		t.Errorf("profile did not round-trip: %+v", p)
	}
	if p.UpdatedAt.IsZero() {
		t.Error("updated_at was not set")
	}

	// Saving again updates rather than duplicating.
	if err := s.SaveOrgProfile(OrgProfile{Org: DefaultOrg, Name: "Zero Moblt Ltd"}); err != nil {
		t.Fatal(err)
	}
	p, _ = s.OrgProfileFor(DefaultOrg)
	if p.Name != "Zero Moblt Ltd" {
		t.Errorf("the update did not apply: %q", p.Name)
	}
}

// The briefing is what the agent is actually given, so it has to read like a
// sentence rather than a struct dump.
func TestTheBriefingReadsAsProse(t *testing.T) {
	b := OrgProfile{
		Name: "Zero Moblt", Domain: "zeromoblt.com",
		Description: "We build AI agents.", Timezone: "Asia/Kolkata",
		Context: "Tickets live in YouTrack.",
	}.Briefing()

	for _, want := range []string{"You work for Zero Moblt", "zeromoblt.com", "We build AI agents.",
		"Asia/Kolkata", "Tickets live in YouTrack."} {
		if !strings.Contains(b, want) {
			t.Errorf("the briefing omits %q:\n%s", want, b)
		}
	}

	// A partially filled profile must not produce dangling fragments.
	only := OrgProfile{Context: "Just some conventions."}.Briefing()
	if strings.Contains(only, "You work for") {
		t.Errorf("a profile with no name claimed one: %q", only)
	}
	if only != "Just some conventions." {
		t.Errorf("context-only briefing has stray formatting: %q", only)
	}
}
