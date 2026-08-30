package store

import "testing"

func TestMapMemberAndLookup(t *testing.T) {
	s := newTestStore(t)
	if err := s.MapMember(Member{ExternalKind: "slack", ExternalID: "U123", Member: "nikhil", Org: "acme", Name: "Nikhil"}); err != nil {
		t.Fatalf("map: %v", err)
	}

	got, ok, err := s.MemberByExternal("slack", "U123")
	if err != nil || !ok || got.Member != "nikhil" || got.Name != "Nikhil" {
		t.Fatalf("lookup: %+v, %v, %v", got, ok, err)
	}

	if _, ok, err := s.MemberByExternal("slack", "unknown"); err != nil || ok {
		t.Errorf("unknown external id: ok=%v err=%v", ok, err)
	}
	// Same external id, different kind, is a distinct mapping.
	if _, ok, err := s.MemberByExternal("jira", "U123"); err != nil || ok {
		t.Errorf("wrong kind matched: ok=%v err=%v", ok, err)
	}
}

func TestMapMemberUpdatesInPlace(t *testing.T) {
	s := newTestStore(t)
	if err := s.MapMember(Member{ExternalKind: "slack", ExternalID: "U123", Member: "nikhil", Name: "Nikhil"}); err != nil {
		t.Fatal(err)
	}
	if err := s.MapMember(Member{ExternalKind: "slack", ExternalID: "U123", Member: "nikhil", Name: "Nikhil G"}); err != nil {
		t.Fatal(err)
	}
	got, ok, err := s.MemberByExternal("slack", "U123")
	if err != nil || !ok || got.Name != "Nikhil G" {
		t.Fatalf("update did not stick: %+v, %v, %v", got, ok, err)
	}

	all, err := s.ListDirectory("slack")
	if err != nil || len(all) != 1 {
		t.Fatalf("re-mapping should not duplicate the row: %+v, %v", all, err)
	}
}

func TestListDirectoryByKind(t *testing.T) {
	s := newTestStore(t)
	if err := s.MapMember(Member{ExternalKind: "slack", ExternalID: "U1", Member: "a"}); err != nil {
		t.Fatal(err)
	}
	if err := s.MapMember(Member{ExternalKind: "slack", ExternalID: "U2", Member: "b"}); err != nil {
		t.Fatal(err)
	}
	if err := s.MapMember(Member{ExternalKind: "jira", ExternalID: "J1", Member: "a"}); err != nil {
		t.Fatal(err)
	}

	slack, err := s.ListDirectory("slack")
	if err != nil || len(slack) != 2 {
		t.Fatalf("ListDirectory(slack): %+v, %v", slack, err)
	}
	all, err := s.ListDirectory("")
	if err != nil || len(all) != 3 {
		t.Fatalf("ListDirectory(\"\"): %+v, %v", all, err)
	}
}

// An operator has to be able to see who the agent can act as, and to take it
// away again. Neither was possible: MapMember existed and nothing read the
// table back or removed a row.
func TestTheDirectoryCanBeListedAndUnmapped(t *testing.T) {
	s := newTestStore(t)
	for _, m := range []Member{
		{ExternalKind: "slack", ExternalID: "U1", Member: "kartik", Name: "Kartik"},
		{ExternalKind: "slack", ExternalID: "U2", Member: "priya"},
		{ExternalKind: "github", ExternalID: "gk", Member: "kartik"},
	} {
		if err := s.MapMember(m); err != nil {
			t.Fatal(err)
		}
	}

	all, err := s.Directory()
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 {
		t.Fatalf("listed %d of 3 mappings", len(all))
	}

	if err := s.UnmapMember("slack", "U1"); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.MemberByExternal("slack", "U1"); ok {
		t.Error("unmapped identity still resolves — the agent can still act as them")
	}
	// Only that one: unmapping must not be a blunt instrument.
	if _, ok, _ := s.MemberByExternal("github", "gk"); !ok {
		t.Error("unmapping one identity removed another")
	}
}
