package store

import (
	"testing"
	"time"
)

func TestQueryAuditFilters(t *testing.T) {
	s := newTestStore(t)

	events := []AuditEvent{
		{ActorKind: "human", ActorID: "u1", Agent: "eng-bot", CaseID: "c1", Verb: "jira.transition", Decision: "allowed"},
		{ActorKind: "agent", ActorID: "eng-bot", Agent: "eng-bot", CaseID: "c1", Verb: "github.merge", Decision: "denied"},
		{ActorKind: "human", ActorID: "u1", Agent: "hr-bot", CaseID: "c2", Verb: "jira.transition", Decision: "allowed"},
	}
	for _, e := range events {
		if err := s.AppendAudit(e); err != nil {
			t.Fatalf("append: %v", err)
		}
	}

	all, err := s.QueryAudit(AuditFilter{})
	if err != nil || len(all) != 3 {
		t.Fatalf("no filter: %+v, %v", all, err)
	}

	byActor, err := s.QueryAudit(AuditFilter{ActorID: "u1"})
	if err != nil || len(byActor) != 2 {
		t.Fatalf("by actor: %+v, %v", byActor, err)
	}

	byVerb, err := s.QueryAudit(AuditFilter{Verb: "github.merge"})
	if err != nil || len(byVerb) != 1 || byVerb[0].CaseID != "c1" {
		t.Fatalf("by verb: %+v, %v", byVerb, err)
	}

	byCombo, err := s.QueryAudit(AuditFilter{ActorID: "u1", Agent: "hr-bot"})
	if err != nil || len(byCombo) != 1 || byCombo[0].CaseID != "c2" {
		t.Fatalf("by actor+agent: %+v, %v", byCombo, err)
	}

	none, err := s.QueryAudit(AuditFilter{CaseID: "no-such-case"})
	if err != nil || len(none) != 0 {
		t.Fatalf("by missing case: %+v, %v", none, err)
	}
}

func TestQueryAuditOrderAndDefaultLimit(t *testing.T) {
	s := newTestStore(t)
	base := time.Now().Add(-time.Hour)
	for i := 0; i < 3; i++ {
		if err := s.AppendAudit(AuditEvent{
			ActorID: "u1", Verb: "v", CreatedAt: base.Add(time.Duration(i) * time.Minute),
		}); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.QueryAudit(AuditFilter{})
	if err != nil || len(got) != 3 {
		t.Fatalf("query: %+v, %v", got, err)
	}
	// Newest first.
	if !got[0].CreatedAt.After(got[1].CreatedAt) || !got[1].CreatedAt.After(got[2].CreatedAt) {
		t.Errorf("audit events not newest-first: %+v", got)
	}

	// Since excludes anything before the cutoff.
	since, err := s.QueryAudit(AuditFilter{Since: base.Add(90 * time.Second)})
	if err != nil || len(since) != 1 {
		t.Fatalf("since filter: %+v, %v", since, err)
	}

	limited, err := s.QueryAudit(AuditFilter{Limit: 1})
	if err != nil || len(limited) != 1 {
		t.Fatalf("explicit limit: %+v, %v", limited, err)
	}
}
