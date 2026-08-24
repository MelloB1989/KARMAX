package jira

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/MelloB1989/karmax/pkg/connectorkit"
)

// reconcileID names the poll source, used as the second half of the stored
// cursor key (connector, source).
const reconcileID = "reconcile"

// reconcileInterval is deliberately not fast: this exists to catch a webhook
// that never arrived, not to replace the webhook as the primary path.
const reconcileInterval = 5 * time.Minute

// pollPageSize bounds one search page.
const pollPageSize = 100

// pollPageCap bounds how much of a backlog one poll cycle will walk, so a huge
// batch of missed updates is worked off over several cycles rather than one
// poll blocking the ticker indefinitely.
const pollPageCap = 20

// pollSafetyMargin re-queries slightly before the stored cursor. Jira's JQL
// date comparison is minute-granularity, so anything updated in the same
// minute as the cursor could otherwise fall on the wrong side of the boundary
// and never be re-queried. The Go-side dedupe below is what actually decides
// what is new, using the full-precision timestamp the margin alone cannot see.
const pollSafetyMargin = time.Minute

// searchJQL builds the JQL for one reconcile sweep. A zero cursor means "never
// polled before" and asks for nothing — the first sweep after a fresh install
// or a long outage must not blast out years of history as caught-up events.
func searchJQL(cursor time.Time) string {
	if cursor.IsZero() {
		return ""
	}
	q := cursor.Add(-pollSafetyMargin).UTC().Format("2006/01/02 15:04")
	return fmt.Sprintf(`updated >= "%s" order by updated asc`, q)
}

// issueSnapshot is one issue's state as reconcile saw it, ready to become an
// event or be deduplicated against another sighting of the same key.
type issueSnapshot struct {
	Updated time.Time
	Payload map[string]any
}

// reconcileDedupe collapses a poll's raw hits into genuinely new sightings.
//
// Two things can otherwise produce a duplicate: the safety margin re-fetching
// an issue already emitted last cycle, and a page boundary returning the same
// issue twice. Both collapse to the same rule — keep only the most recent
// snapshot per key, and only when it is strictly newer than the cursor — so an
// issue seen twice produces one event.
func reconcileDedupe(cursor time.Time, hits []issueSnapshot) (events []map[string]any, next time.Time) {
	latest := map[string]issueSnapshot{}
	next = cursor
	for _, h := range hits {
		if !h.Updated.After(cursor) {
			continue
		}
		if prev, ok := latest[keyOf(h)]; !ok || h.Updated.After(prev.Updated) {
			latest[keyOf(h)] = h
		}
		if h.Updated.After(next) {
			next = h.Updated
		}
	}
	keys := make([]string, 0, len(latest))
	for k := range latest {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		events = append(events, latest[k].Payload)
	}
	return events, next
}

func keyOf(h issueSnapshot) string {
	k, _ := h.Payload["key"].(string)
	return k
}

type searchRequest struct {
	JQL        string   `json:"jql"`
	StartAt    int      `json:"startAt"`
	MaxResults int      `json:"maxResults"`
	Fields     []string `json:"fields"`
	Expand     []string `json:"expand"`
}

type searchResponse struct {
	Total  int         `json:"total"`
	Issues []issueJSON `json:"issues"`
}

var searchFields = []string{
	"summary", "status", "assignee", "reporter", "project", "issuetype", "created", "updated",
}

// reconcilePoll is the EventSource.Poll for the reconcile sweep.
//
// It only ever produces jira.issue.updated: connectorkit gives one static
// event kind per source, and distinguishing "reconcile found a brand-new
// issue" from "reconcile found a moved one" would need a second sweep with its
// own API calls for a case (a missed *creation*, as opposed to a missed
// *transition*) that recipes have not asked to await on. What recipes await is
// a status landing somewhere, which is exactly what this recovers.
func reconcilePoll(ctx context.Context, cr connectorkit.Credentials, cursor string) ([]map[string]any, string, error) {
	since, err := parseCursor(cursor)
	if err != nil {
		return nil, "", err
	}
	if since.IsZero() {
		// First run: start the clock rather than replaying history.
		return nil, formatCursor(time.Now().UTC()), nil
	}

	jql := searchJQL(since)
	var hits []issueSnapshot
	for page := 0; page < pollPageCap; page++ {
		req := searchRequest{
			JQL: jql, StartAt: page * pollPageSize, MaxResults: pollPageSize,
			Fields: searchFields, Expand: []string{"changelog"},
		}
		body, err := json.Marshal(req)
		if err != nil {
			return nil, "", err
		}
		var res searchResponse
		if _, err := call(ctx, cr, "POST", "/rest/api/3/search", body, &res); err != nil {
			return nil, "", err
		}
		for _, iss := range res.Issues {
			snap, err := snapshotOf(iss, since)
			if err != nil {
				continue // an unparseable timestamp must not sink the whole sweep
			}
			hits = append(hits, snap)
		}
		if len(res.Issues) < pollPageSize || page*pollPageSize+len(res.Issues) >= res.Total {
			break
		}
	}

	events, next := reconcileDedupe(since, hits)
	return events, formatCursor(next), nil
}

// snapshotOf turns one search result into a snapshot with the same payload
// shape the webhook path emits, so a recipe's match: works identically
// whichever path delivered the event.
func snapshotOf(iss issueJSON, since time.Time) (issueSnapshot, error) {
	updated, err := parseJiraTime(iss.Fields.Updated)
	if err != nil {
		return issueSnapshot{}, err
	}
	status := iss.Fields.Status.Name
	assignee := iss.Fields.Assignee.name()
	prevStatus, prevAssignee, changed := historySince(iss.Changelog, since, status, assignee)

	payload := map[string]any{
		"key":               iss.Key,
		"summary":           iss.Fields.Summary,
		"status":            status,
		"previous_status":   prevStatus,
		"assignee":          assignee,
		"previous_assignee": prevAssignee,
		"reporter":          iss.Fields.Reporter.name(),
		"project":           iss.Fields.Project.Key,
		"project_name":      iss.Fields.Project.Name,
		"issue_type":        iss.Fields.IssueType.Name,
		"url":               browseURL(iss.Self, iss.Key),
		"actor":             "", // a sweep has no single actor; the webhook path is where that lives
		"changed_fields":    changed,
	}
	return issueSnapshot{Updated: updated, Payload: payload}, nil
}

// historySince walks a search-with-changelog result's full history — oldest
// first, as Jira returns it — for what changed after the cursor, so a
// reconciled event carries the same "what moved from what" shape a webhook
// delta would have.
func historySince(cl *changelogJSON, since time.Time, currentStatus, currentAssignee string) (prevStatus, prevAssignee string, changed []string) {
	prevStatus, prevAssignee = currentStatus, currentAssignee
	if cl == nil {
		return
	}
	foundStatus, foundAssignee := false, false
	seen := map[string]bool{}
	for _, h := range cl.Histories {
		at, err := parseJiraTime(h.Created)
		if err != nil || !at.After(since) {
			continue
		}
		for _, it := range h.Items {
			if !seen[it.Field] {
				changed = append(changed, it.Field)
				seen[it.Field] = true
			}
			switch it.Field {
			case "status":
				if !foundStatus {
					prevStatus, foundStatus = it.FromString, true
				}
			case "assignee":
				if !foundAssignee {
					prevAssignee, foundAssignee = it.FromString, true
				}
			}
		}
	}
	return
}

func parseCursor(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	return time.Parse(time.RFC3339Nano, s)
}

func formatCursor(t time.Time) string {
	return t.UTC().Format(time.RFC3339Nano)
}
