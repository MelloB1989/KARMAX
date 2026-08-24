package jira

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestSearchJQLOnAFreshCursorAsksForNothing(t *testing.T) {
	// A zero cursor means "never polled before" — searchJQL itself is not
	// what protects against a history blast (reconcilePoll short-circuits
	// before calling it), but it must still not silently produce "everything".
	if got := searchJQL(time.Time{}); got != "" {
		t.Errorf("zero cursor produced JQL %q", got)
	}
}

func TestSearchJQLAppliesTheSafetyMargin(t *testing.T) {
	cursor := time.Date(2024, 3, 15, 10, 30, 0, 0, time.UTC)
	got := searchJQL(cursor)
	want := `updated >= "2024/03/15 10:29" order by updated asc`
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestReconcileDedupeKeepsOnlyTheLatestPerKeyAfterTheCursor(t *testing.T) {
	cursor := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	hits := []issueSnapshot{
		{Updated: cursor.Add(-time.Hour), Payload: map[string]any{"key": "OLD-1"}},            // before cursor: dropped
		{Updated: cursor.Add(time.Minute), Payload: map[string]any{"key": "A-1", "v": 1}},     // first sighting
		{Updated: cursor.Add(2 * time.Minute), Payload: map[string]any{"key": "A-1", "v": 2}}, // same issue again, newer
		{Updated: cursor.Add(3 * time.Minute), Payload: map[string]any{"key": "B-1", "v": 1}},
	}
	events, next := reconcileDedupe(cursor, hits)
	if len(events) != 2 {
		t.Fatalf("got %d events, want one per distinct key: %+v", len(events), events)
	}
	byKey := map[string]map[string]any{}
	for _, e := range events {
		byKey[e["key"].(string)] = e
	}
	if byKey["A-1"]["v"] != 2 {
		t.Errorf("A-1 kept version %v, want the newer sighting (2)", byKey["A-1"]["v"])
	}
	if !next.Equal(cursor.Add(3 * time.Minute)) {
		t.Errorf("next cursor = %v, want the max updated seen", next)
	}
}

func TestReconcileDedupeWithNothingNewAdvancesNothing(t *testing.T) {
	cursor := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	hits := []issueSnapshot{{Updated: cursor, Payload: map[string]any{"key": "A-1"}}} // not strictly after
	events, next := reconcileDedupe(cursor, hits)
	if len(events) != 0 || !next.Equal(cursor) {
		t.Errorf("events=%v next=%v, want no advance on a boundary-equal hit", events, next)
	}
}

func TestHistorySinceFindsTheEarliestStatusChangeAfterCursor(t *testing.T) {
	cursor := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	cl := &changelogJSON{Histories: []changelogHistory{
		{Created: "2023-12-31T00:00:00.000+0000", Items: []changelogItem{
			{Field: "status", FromString: "Backlog", ToString: "To Do"}, // before cursor: ignored
		}},
		{Created: "2024-01-01T01:00:00.000+0000", Items: []changelogItem{
			{Field: "status", FromString: "To Do", ToString: "In Progress"},
		}},
		{Created: "2024-01-01T02:00:00.000+0000", Items: []changelogItem{
			{Field: "status", FromString: "In Progress", ToString: "Done"},
			{Field: "assignee", FromString: "", ToString: "Mia"},
		}},
	}}
	prevStatus, prevAssignee, changed := historySince(cl, cursor, "Done", "Mia")
	if prevStatus != "To Do" {
		t.Errorf("previous status = %q, want the status before the FIRST post-cursor change", prevStatus)
	}
	if prevAssignee != "" {
		t.Errorf("previous assignee = %q, want empty (first assignment)", prevAssignee)
	}
	if len(changed) != 2 {
		t.Errorf("changed = %v, want status and assignee once each", changed)
	}
}

func TestHistorySinceWithNoChangelogLeavesCurrentAsPrevious(t *testing.T) {
	prevStatus, prevAssignee, changed := historySince(nil, time.Now(), "Done", "Mia")
	if prevStatus != "Done" || prevAssignee != "Mia" || changed != nil {
		t.Errorf("got %q %q %v", prevStatus, prevAssignee, changed)
	}
}

func TestCursorRoundTrips(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Millisecond)
	if got, err := parseCursor(formatCursor(now)); err != nil || !got.Equal(now) {
		t.Errorf("round trip: got %v err %v, want %v", got, err, now)
	}
	if got, err := parseCursor(""); err != nil || !got.IsZero() {
		t.Errorf("empty cursor: got %v err %v", got, err)
	}
	if _, err := parseCursor("not a time"); err == nil {
		t.Error("a malformed stored cursor was accepted")
	}
}

func TestReconcilePollFirstRunSeedsWithoutReplayingHistory(t *testing.T) {
	events, next, err := reconcilePoll(context.Background(), creds(nil), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 0 {
		t.Errorf("a never-before-polled cursor emitted %d events, want a silent seed", len(events))
	}
	if next == "" {
		t.Error("no cursor was seeded")
	}
}

func TestReconcilePollEmitsAndAdvancesPastEmittedIssues(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req searchRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.JQL == "" {
			t.Error("reconcile called search with no JQL on a non-empty cursor")
		}
		_, _ = w.Write([]byte(`{
			"total": 1,
			"issues": [{
				"key": "ED-9",
				"self": "https://acme.atlassian.net/rest/api/3/issue/9",
				"fields": {
					"summary": "reconciled ticket",
					"status": {"name": "Prioritized"},
					"project": {"key": "ED", "name": "Edison Project"},
					"updated": "2024-01-02T00:05:00.000+0000"
				}
			}]
		}`))
	}))
	defer srv.Close()

	cursor := time.Date(2024, 1, 2, 0, 0, 0, 0, time.UTC)
	c := creds(map[string]string{"email": "a@b.com", "api_token": "t", "site_url": srv.URL})
	events, next, err := reconcilePoll(context.Background(), c, formatCursor(cursor))
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0]["key"] != "ED-9" || events[0]["status"] != "Prioritized" {
		t.Fatalf("events = %+v", events)
	}
	if next == formatCursor(cursor) {
		t.Error("cursor did not advance past the emitted issue")
	}
}
