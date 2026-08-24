package jira

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/MelloB1989/karmax/internal/safety"
	"github.com/MelloB1989/karmax/pkg/connectorkit"
)

func (c *Connector) toolManifests() []connectorkit.Tool {
	return []connectorkit.Tool{
		{
			Name: c.name("jira.search"),
			Description: "Search Jira issues with JQL — the query language for filtering by project, status, " +
				"assignee, text and more. Use this to find tickets rather than guessing keys, e.g. " +
				`jql="project = ENG AND status = 'In Progress' ORDER BY updated DESC".`,
			Parameters: json.RawMessage(`{
				"type":"object",
				"properties":{
					"jql":{"type":"string","description":"A JQL query. Required."},
					"limit":{"type":"integer","description":"Maximum results (default 25, max 100)."}
				},
				"required":["jql"]
			}`),
			Call: search,
		},
		{
			Name: c.name("jira.issue"),
			Description: "Read one Jira issue in full — summary, description, status, assignee, reporter and " +
				"comment count. Takes the issue key, e.g. PROJ-123.",
			Parameters: json.RawMessage(`{
				"type":"object",
				"properties":{"key":{"type":"string","description":"The issue key, e.g. PROJ-123."}},
				"required":["key"]
			}`),
			Call: getIssue,
		},
		{
			Name:        c.name("jira.comment"),
			Description: "Add a comment to a Jira issue. Use to leave a note, ask a question, or report progress on a ticket.",
			Parameters: json.RawMessage(`{
				"type":"object",
				"properties":{
					"key":{"type":"string","description":"The issue key, e.g. PROJ-123."},
					"body":{"type":"string","description":"The comment text."}
				},
				"required":["key","body"]
			}`),
			Call: comment,
		},
		{
			Name: c.name("jira.transitions"),
			Description: "List the transitions available on one issue right now — the workflow moves it can make " +
				"and the exact name each needs. Call this before jira.transition: workflows differ by project and " +
				"issue type, so a status name is never worth guessing.",
			Parameters: json.RawMessage(`{
				"type":"object",
				"properties":{"key":{"type":"string","description":"The issue key, e.g. PROJ-123."}},
				"required":["key"]
			}`),
			Call: listTransitions,
		},
		{
			Name: c.name("jira.transition"),
			Description: "Move a Jira issue to a different status. Takes the transition NAME as shown by " +
				"jira.transitions (an id also works), not the status name — several transitions can lead to the " +
				"same status, and only one of them is valid from where the issue is now.",
			Parameters: json.RawMessage(`{
				"type":"object",
				"properties":{
					"key":{"type":"string","description":"The issue key, e.g. PROJ-123."},
					"transition":{"type":"string","description":"A transition name or id from jira.transitions."},
					"comment":{"type":"string","description":"Optional comment to attach to the move."}
				},
				"required":["key","transition"]
			}`),
			Call: c.doTransition,
		},
	}
}

func search(ctx context.Context, cr connectorkit.Credentials, in map[string]any) (any, error) {
	jql, _ := in["jql"].(string)
	if strings.TrimSpace(jql) == "" {
		return nil, fmt.Errorf("jira: jql is required")
	}
	limit := intOf(in["limit"], 25)
	if limit > 100 {
		limit = 100
	}

	body, err := json.Marshal(searchRequest{JQL: jql, MaxResults: limit, Fields: searchFields})
	if err != nil {
		return nil, err
	}
	var res searchResponse
	if _, err := call(ctx, cr, http.MethodPost, "/rest/api/3/search", body, &res); err != nil {
		return nil, err
	}

	items := make([]map[string]any, 0, len(res.Issues))
	for _, iss := range res.Issues {
		items = append(items, map[string]any{
			"key": iss.Key, "status": iss.Fields.Status.Name,
			"assignee": iss.Fields.Assignee.name(), "project": iss.Fields.Project.Key,
			"issue_type": iss.Fields.IssueType.Name, "updated": iss.Fields.Updated,
			"url": browseURL(iss.Self, iss.Key),
			// Written by whoever filed the ticket, fenced before it reaches a model.
			"summary": safety.Fence("a Jira issue ("+iss.Key+")", iss.Fields.Summary),
		})
	}
	return map[string]any{"jql": jql, "count": len(items), "total": res.Total, "issues": items}, nil
}

func getIssue(ctx context.Context, cr connectorkit.Credentials, in map[string]any) (any, error) {
	key, err := keyOfArg(in)
	if err != nil {
		return nil, err
	}
	var iss issueJSON
	path := "/rest/api/3/issue/" + key +
		"?fields=summary,description,status,assignee,reporter,project,issuetype,comment,created,updated"
	if _, err := call(ctx, cr, http.MethodGet, path, nil, &iss); err != nil {
		return nil, err
	}
	source := "a Jira issue (" + iss.Key + ")"
	return map[string]any{
		"key": iss.Key, "status": iss.Fields.Status.Name,
		"assignee": iss.Fields.Assignee.name(), "reporter": iss.Fields.Reporter.name(),
		"project": iss.Fields.Project.Key, "project_name": iss.Fields.Project.Name,
		"issue_type": iss.Fields.IssueType.Name, "created": iss.Fields.Created,
		"updated": iss.Fields.Updated, "comments": iss.Fields.Comment.Total,
		"url": browseURL(iss.Self, iss.Key),
		// Both written by whoever filed the ticket — could be an external
		// reporter — so both are fenced before a model reads them.
		"summary":     safety.Fence(source, iss.Fields.Summary),
		"description": safety.Fence(source, adfText(iss.Fields.Description)),
	}, nil
}

func comment(ctx context.Context, cr connectorkit.Credentials, in map[string]any) (any, error) {
	key, err := keyOfArg(in)
	if err != nil {
		return nil, err
	}
	body, _ := in["body"].(string)
	if strings.TrimSpace(body) == "" {
		return nil, fmt.Errorf("jira: body is required")
	}
	payload, err := json.Marshal(map[string]any{"body": plainADF(body)})
	if err != nil {
		return nil, err
	}
	var out struct {
		ID string `json:"id"`
	}
	if _, err := call(ctx, cr, http.MethodPost, "/rest/api/3/issue/"+key+"/comment", payload, &out); err != nil {
		return nil, err
	}
	return map[string]any{"key": key, "comment_id": out.ID, "posted": true}, nil
}

type transitionJSON struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	To   struct {
		Name string `json:"name"`
	} `json:"to"`
}

func listTransitions(ctx context.Context, cr connectorkit.Credentials, in map[string]any) (any, error) {
	key, err := keyOfArg(in)
	if err != nil {
		return nil, err
	}
	ts, err := transitionsOf(ctx, cr, key)
	if err != nil {
		return nil, err
	}
	items := make([]map[string]any, 0, len(ts))
	for _, t := range ts {
		items = append(items, map[string]any{"id": t.ID, "name": t.Name, "to_status": t.To.Name})
	}
	return map[string]any{"key": key, "transitions": items}, nil
}

// doTransition is a method so it can look up the tool's own qualified name in
// its error message — helpful when several Jira accounts are connected and a
// transition fails on the wrong one.
func (c *Connector) doTransition(ctx context.Context, cr connectorkit.Credentials, in map[string]any) (any, error) {
	key, err := keyOfArg(in)
	if err != nil {
		return nil, err
	}
	want, _ := in["transition"].(string)
	if strings.TrimSpace(want) == "" {
		return nil, fmt.Errorf("jira: transition is required")
	}

	ts, err := transitionsOf(ctx, cr, key)
	if err != nil {
		return nil, err
	}
	id := resolveTransition(ts, want)
	if id == "" {
		names := make([]string, len(ts))
		for i, t := range ts {
			names[i] = t.Name
		}
		return nil, fmt.Errorf("jira: %q is not a transition available on %s right now (available: %s) — call %s first",
			want, key, strings.Join(names, ", "), c.name("jira.transitions"))
	}

	req := map[string]any{"transition": map[string]string{"id": id}}
	if body, _ := in["comment"].(string); strings.TrimSpace(body) != "" {
		req["update"] = map[string]any{
			"comment": []map[string]any{{"add": map[string]any{"body": plainADF(body)}}},
		}
	}
	payload, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	if _, err := call(ctx, cr, http.MethodPost, "/rest/api/3/issue/"+key+"/transitions", payload, nil); err != nil {
		return nil, err
	}
	return map[string]any{"key": key, "transitioned": true}, nil
}

func transitionsOf(ctx context.Context, cr connectorkit.Credentials, key string) ([]transitionJSON, error) {
	var out struct {
		Transitions []transitionJSON `json:"transitions"`
	}
	if _, err := call(ctx, cr, http.MethodGet, "/rest/api/3/issue/"+key+"/transitions", nil, &out); err != nil {
		return nil, err
	}
	return out.Transitions, nil
}

// resolveTransition matches a name case-insensitively before falling back to
// an id, so an agent that read jira.transitions can pass either back.
func resolveTransition(ts []transitionJSON, want string) string {
	for _, t := range ts {
		if strings.EqualFold(t.Name, want) {
			return t.ID
		}
	}
	for _, t := range ts {
		if t.ID == want {
			return t.ID
		}
	}
	return ""
}

func keyOfArg(in map[string]any) (string, error) {
	key, _ := in["key"].(string)
	key = strings.TrimSpace(key)
	if key == "" {
		return "", fmt.Errorf("jira: key is required")
	}
	return key, nil
}
