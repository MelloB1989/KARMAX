package store

import "testing"

func TestOpenCaseIsIdempotent(t *testing.T) {
	s := newTestStore(t)

	first, err := s.OpenCase(Case{Org: "acme", Agent: "eng-bot", Key: "jira:ACME-1", Title: "first title", State: "open"})
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	// Move the state on, as the workflow would between the webhook's first and
	// second delivery.
	if err := s.SetCaseState(first.ID, "grooming"); err != nil {
		t.Fatalf("set state: %v", err)
	}

	// A redelivered webhook must rejoin the same case rather than reset it —
	// same id, same (moved-on) state, same title.
	second, err := s.OpenCase(Case{Org: "acme", Agent: "eng-bot", Key: "jira:ACME-1", Title: "a different title", State: "open"})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if second.ID != first.ID {
		t.Errorf("reopen minted a new id: %q vs %q", second.ID, first.ID)
	}
	if second.State != "grooming" {
		t.Errorf("reopen reset state to %q", second.State)
	}
	if second.Title != "first title" {
		t.Errorf("reopen overwrote title with %q", second.Title)
	}
}

func TestCaseByKeyAndID(t *testing.T) {
	s := newTestStore(t)
	c, err := s.OpenCase(Case{Key: "k1", Title: "t", Agent: "a"})
	if err != nil {
		t.Fatal(err)
	}

	byKey, ok, err := s.CaseByKey("k1")
	if err != nil || !ok || byKey.ID != c.ID {
		t.Fatalf("CaseByKey = %+v, %v, %v", byKey, ok, err)
	}
	byID, ok, err := s.CaseByID(c.ID)
	if err != nil || !ok || byID.Key != "k1" {
		t.Fatalf("CaseByID = %+v, %v, %v", byID, ok, err)
	}

	if _, ok, err := s.CaseByKey("missing"); err != nil || ok {
		t.Errorf("CaseByKey on a missing key: ok=%v err=%v", ok, err)
	}
}

func TestBindCaseThreadAndListCases(t *testing.T) {
	s := newTestStore(t)
	c, err := s.OpenCase(Case{Key: "k1", Agent: "eng-bot", State: "open"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.BindCaseThread(c.ID, "#eng", "1234.5678"); err != nil {
		t.Fatalf("bind thread: %v", err)
	}
	got, _, err := s.CaseByID(c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ThreadChannel != "#eng" || got.ThreadTS != "1234.5678" {
		t.Errorf("thread binding did not stick: %+v", got)
	}

	if _, err := s.OpenCase(Case{Key: "k2", Agent: "eng-bot", State: "review"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.OpenCase(Case{Key: "k3", Agent: "other-bot", State: "open"}); err != nil {
		t.Fatal(err)
	}

	byAgent, err := s.ListCases("eng-bot", "", 10)
	if err != nil || len(byAgent) != 2 {
		t.Fatalf("ListCases by agent: %+v, %v", byAgent, err)
	}
	byState, err := s.ListCases("", "open", 10)
	if err != nil || len(byState) != 2 {
		t.Fatalf("ListCases by state: %+v, %v", byState, err)
	}
	byBoth, err := s.ListCases("eng-bot", "open", 10)
	if err != nil || len(byBoth) != 1 || byBoth[0].Key != "k1" {
		t.Fatalf("ListCases by agent+state: %+v, %v", byBoth, err)
	}
}

func TestCaseHistory(t *testing.T) {
	s := newTestStore(t)
	c, err := s.OpenCase(Case{Key: "k1"})
	if err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"note", "state-change", "note"} {
		if err := s.AppendCaseEvent(CaseEvent{CaseID: c.ID, Kind: kind, Payload: kind, Actor: "agent"}); err != nil {
			t.Fatalf("append event: %v", err)
		}
	}
	hist, err := s.CaseHistory(c.ID, 10)
	if err != nil || len(hist) != 3 {
		t.Fatalf("history: %+v, %v", hist, err)
	}
	// Events for a different case must not leak in.
	other, err := s.OpenCase(Case{Key: "k2"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AppendCaseEvent(CaseEvent{CaseID: other.ID, Kind: "note"}); err != nil {
		t.Fatal(err)
	}
	hist, err = s.CaseHistory(c.ID, 10)
	if err != nil || len(hist) != 3 {
		t.Fatalf("history leaked another case's events: %+v", hist)
	}
}
