package store

import (
	"testing"
	"time"
)

// A restart is not new information. An integration healthy a minute before a
// deploy is almost certainly still healthy after it.
func TestAHealthVerdictSurvivesReopening(t *testing.T) {
	s := newTestStore(t)

	if err := s.SaveConnectorHealth(ConnectorHealth{
		Connector: "slack", Status: "healthy", Detail: "Reachable",
	}); err != nil {
		t.Fatal(err)
	}

	got, err := s.ConnectorHealthFor("slack")
	if err != nil || got == nil {
		t.Fatalf("verdict not stored: %v", err)
	}
	if got.Status != "healthy" || got.Detail != "Reachable" {
		t.Errorf("verdict changed: %+v", got)
	}
	if got.CheckedAt.IsZero() {
		t.Error("checked_at was not set, so nothing can tell how old this is")
	}

	// Re-checking updates in place rather than accumulating rows.
	if err := s.SaveConnectorHealth(ConnectorHealth{
		Connector: "slack", Status: "failed", Detail: "token rejected",
	}); err != nil {
		t.Fatal(err)
	}
	all, err := s.AllConnectorHealth()
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 || all["slack"].Status != "failed" {
		t.Errorf("expected one updated row, got %+v", all)
	}
}

func TestAnUncheckedConnectorHasNoVerdict(t *testing.T) {
	s := newTestStore(t)
	got, err := s.ConnectorHealthFor("never-checked")
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Errorf("a connector that was never checked reported %+v", got)
	}
}

// A verdict presented as current when it is hours old is worse than one
// labelled old — the difference matters when deciding whether an integration
// is the cause of a problem.
func TestStalenessIsDetectable(t *testing.T) {
	fresh := ConnectorHealth{CheckedAt: time.Now()}
	if fresh.Stale(time.Hour) {
		t.Error("a verdict from just now was called stale")
	}
	old := ConnectorHealth{CheckedAt: time.Now().Add(-2 * time.Hour)}
	if !old.Stale(time.Hour) {
		t.Error("a two-hour-old verdict was called current")
	}
	if !(ConnectorHealth{}).Stale(time.Hour) {
		t.Error("a verdict with no timestamp should count as stale")
	}
}
