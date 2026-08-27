package youtrack

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/MelloB1989/karmax/pkg/connectorkit"
)

// issueFields is what every read returns.
//
// YouTrack returns NOTHING but the id unless you enumerate fields, so this
// constant is the difference between a useful answer and `{"id":"2-14"}`.
const issueFields = "idReadable,summary,description,created,updated,resolved," +
	"project(shortName,name),reporter(login,fullName),updater(login)," +
	"customFields(name,value(name,login,fullName,text,minutes))"

type issue struct {
	IDReadable  string `json:"idReadable"`
	Summary     string `json:"summary"`
	Description string `json:"description"`
	Created     int64  `json:"created"`
	Updated     int64  `json:"updated"`
	Resolved    *int64 `json:"resolved"`
	Project     struct {
		ShortName string `json:"shortName"`
		Name      string `json:"name"`
	} `json:"project"`
	Reporter struct {
		Login    string `json:"login"`
		FullName string `json:"fullName"`
	} `json:"reporter"`
	CustomFields []struct {
		Name  string          `json:"name"`
		Value json.RawMessage `json:"value"`
	} `json:"customFields"`
}

// flatten renders an issue as the agent should see it: named fields, no
// YouTrack-internal ids, timestamps as RFC3339 rather than epoch millis.
func (i issue) flatten() map[string]any {
	out := map[string]any{
		"id":      i.IDReadable,
		"summary": i.Summary,
		"project": i.Project.ShortName,
		"created": millisToRFC3339(i.Created),
		"updated": millisToRFC3339(i.Updated),
	}
	if i.Description != "" {
		out["description"] = truncate(i.Description, 4000)
	}
	if i.Reporter.Login != "" {
		out["reporter"] = i.Reporter.Login
	}
	out["resolved"] = i.Resolved != nil
	for _, f := range i.CustomFields {
		if v := simpleValue(f.Value); v != nil {
			out[strings.ToLower(strings.ReplaceAll(f.Name, " ", "_"))] = v
		}
	}
	return out
}

// simpleValue pulls the human-meaningful part out of a custom field.
//
// A YouTrack custom field is a tagged union — a state is {name}, an assignee is
// {login,fullName}, an estimate is {minutes}, a text field is {text}, and a
// plain value is a bare scalar. Handing the raw JSON to a model wastes context
// on wrappers and invites it to quote an internal id back at a human.
func simpleValue(raw json.RawMessage) any {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var obj struct {
		Name     string `json:"name"`
		Login    string `json:"login"`
		FullName string `json:"fullName"`
		Text     string `json:"text"`
		Minutes  *int   `json:"minutes"`
	}
	if err := json.Unmarshal(raw, &obj); err == nil {
		switch {
		case obj.Name != "":
			return obj.Name
		case obj.Login != "":
			return obj.Login
		case obj.FullName != "":
			return obj.FullName
		case obj.Text != "":
			return obj.Text
		case obj.Minutes != nil:
			return *obj.Minutes
		}
	}
	var scalar any
	if err := json.Unmarshal(raw, &scalar); err == nil {
		// An array of values (multi-value fields) collapses to its names.
		if arr, ok := scalar.([]any); ok {
			var names []string
			for _, el := range arr {
				if m, ok := el.(map[string]any); ok {
					for _, k := range []string{"name", "login", "fullName"} {
						if s, ok := m[k].(string); ok && s != "" {
							names = append(names, s)
							break
						}
					}
				}
			}
			if len(names) == 0 {
				return nil
			}
			return names
		}
		return scalar
	}
	return nil
}

func (c *Connector) Tools() []connectorkit.Tool {
	return []connectorkit.Tool{
		{
			Name: c.name("youtrack.search"),
			Description: "Find YouTrack issues using YouTrack query syntax, e.g. " +
				"`project: LAMB State: Open`, `assignee: me #Unresolved`, `created: {This week}`. " +
				"Use this before reading or updating, since issue ids are not guessable.",
			Parameters: json.RawMessage(`{
				"type":"object",
				"properties":{
					"query":{"type":"string","description":"YouTrack query. Empty returns the most recently updated issues."},
					"limit":{"type":"integer","description":"Maximum issues to return (default 20, max 100)."}
				}
			}`),
			Call: search,
		},
		{
			Name:        c.name("youtrack.issue"),
			Description: "Read one YouTrack issue in full, including its description and current state.",
			Parameters: json.RawMessage(`{
				"type":"object",
				"properties":{"id":{"type":"string","description":"The readable id, e.g. LAMB-42."}},
				"required":["id"]
			}`),
			Call: readIssue,
		},
		{
			Name: c.name("youtrack.create"),
			Description: "File a new YouTrack issue. Use the project's short name (e.g. LAMB). " +
				"Prefer a specific, actionable summary over a vague one.",
			Parameters: json.RawMessage(`{
				"type":"object",
				"properties":{
					"summary":{"type":"string","description":"One-line title."},
					"description":{"type":"string","description":"Body, markdown."},
					"project":{"type":"string","description":"Project short name. Falls back to the configured default."}
				},
				"required":["summary"]
			}`),
			Call: createIssue,
		},
		{
			Name:        c.name("youtrack.comment"),
			Description: "Add a comment to an existing YouTrack issue.",
			Parameters: json.RawMessage(`{
				"type":"object",
				"properties":{
					"id":{"type":"string","description":"The readable id, e.g. LAMB-42."},
					"text":{"type":"string","description":"Comment body, markdown."}
				},
				"required":["id","text"]
			}`),
			Call: comment,
		},
		{
			Name: c.name("youtrack.command"),
			Description: "Apply a YouTrack command to an issue — this is how state changes, " +
				"assignment and tagging are done. Examples: `State: In Progress`, " +
				"`for: alice`, `Priority: Critical`, `tag Regression`.",
			Parameters: json.RawMessage(`{
				"type":"object",
				"properties":{
					"id":{"type":"string","description":"The readable id, e.g. LAMB-42."},
					"command":{"type":"string","description":"A YouTrack command string."}
				},
				"required":["id","command"]
			}`),
			Call: command,
		},
		{
			Name: c.name("youtrack.projects"),
			Description: "List the YouTrack projects this token can see, with their short names. " +
				"Use when you need a project key and were not given one.",
			Parameters: json.RawMessage(`{"type":"object","properties":{}}`),
			Call:       projects,
		},
	}
}

func search(ctx context.Context, cr connectorkit.Credentials, in map[string]any) (any, error) {
	limit := intArg(in, "limit", 20)
	if limit > 100 {
		limit = 100
	}
	q := strArg(in, "query")

	path := fmt.Sprintf("/issues?fields=%s&$top=%d&query=%s",
		issueFields, limit, urlQuery(q))

	var issues []issue
	if err := call(ctx, cr, "GET", path, nil, &issues); err != nil {
		return nil, err
	}
	out := make([]map[string]any, 0, len(issues))
	for _, i := range issues {
		out = append(out, i.flatten())
	}
	// The count is returned alongside so the model can tell "nothing matched"
	// from "the tool failed" without inferring it from an empty list.
	return map[string]any{"count": len(out), "issues": out}, nil
}

func readIssue(ctx context.Context, cr connectorkit.Credentials, in map[string]any) (any, error) {
	id := strArg(in, "id")
	if id == "" {
		return nil, fmt.Errorf("youtrack: id is required")
	}
	var i issue
	if err := call(ctx, cr, "GET", "/issues/"+urlPath(id)+"?fields="+issueFields, nil, &i); err != nil {
		return nil, err
	}
	return i.flatten(), nil
}

func createIssue(ctx context.Context, cr connectorkit.Credentials, in map[string]any) (any, error) {
	summary := strArg(in, "summary")
	if summary == "" {
		return nil, fmt.Errorf("youtrack: summary is required")
	}
	project := strArg(in, "project")
	if project == "" {
		project = strings.TrimSpace(cr.Get("default_project"))
	}
	if project == "" {
		return nil, fmt.Errorf("youtrack: no project given and no default_project configured — " +
			"call youtrack.projects to see the options")
	}

	body := map[string]any{
		"summary": summary,
		"project": map[string]any{"shortName": project},
	}
	if d := strArg(in, "description"); d != "" {
		body["description"] = d
	}

	var created issue
	if err := call(ctx, cr, "POST", "/issues?fields="+issueFields, body, &created); err != nil {
		return nil, err
	}
	return created.flatten(), nil
}

func comment(ctx context.Context, cr connectorkit.Credentials, in map[string]any) (any, error) {
	id, text := strArg(in, "id"), strArg(in, "text")
	if id == "" || text == "" {
		return nil, fmt.Errorf("youtrack: id and text are both required")
	}
	var res struct {
		ID string `json:"id"`
	}
	if err := call(ctx, cr, "POST", "/issues/"+urlPath(id)+"/comments?fields=id",
		map[string]any{"text": text}, &res); err != nil {
		return nil, err
	}
	return map[string]any{"commented": true, "issue": id}, nil
}

func command(ctx context.Context, cr connectorkit.Credentials, in map[string]any) (any, error) {
	id, cmd := strArg(in, "id"), strArg(in, "command")
	if id == "" || cmd == "" {
		return nil, fmt.Errorf("youtrack: id and command are both required")
	}
	body := map[string]any{
		"query":  cmd,
		"issues": []map[string]any{{"idReadable": id}},
	}
	if err := call(ctx, cr, "POST", "/commands", body, nil); err != nil {
		return nil, err
	}
	// Read the issue back rather than reporting success from a 200: a YouTrack
	// command that matches no field is accepted and does nothing, so "it
	// worked" has to be observed, not assumed.
	var i issue
	if err := call(ctx, cr, "GET", "/issues/"+urlPath(id)+"?fields="+issueFields, nil, &i); err != nil {
		return map[string]any{"applied": true, "issue": id}, nil
	}
	return map[string]any{"applied": true, "issue": i.flatten()}, nil
}

func projects(ctx context.Context, cr connectorkit.Credentials, _ map[string]any) (any, error) {
	var ps []struct {
		ShortName string `json:"shortName"`
		Name      string `json:"name"`
	}
	if err := call(ctx, cr, "GET", "/admin/projects?fields=shortName,name&$top=200", nil, &ps); err != nil {
		return nil, err
	}
	out := make([]map[string]any, 0, len(ps))
	for _, p := range ps {
		out = append(out, map[string]any{"key": p.ShortName, "name": p.Name})
	}
	return map[string]any{"count": len(out), "projects": out}, nil
}
