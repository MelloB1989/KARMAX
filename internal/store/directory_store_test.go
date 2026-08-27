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
