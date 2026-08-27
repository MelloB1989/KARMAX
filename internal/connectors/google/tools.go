package google

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/MelloB1989/karmax/pkg/connectorkit"
)

// Every tool here runs AS THE ACTING EMPLOYEE. The credentials arrive already
// resolved to one person by the connector host, so nothing in this file
// chooses whose data it touches — and nothing in it takes a "user" argument
// the model could fill in with somebody else's name.

func get(ctx context.Context, cr connectorkit.Credentials, endpoint string, q url.Values, out any) error {
	full := endpoint
	if len(q) > 0 {
		full += "?" + q.Encode()
	}
	return do(ctx, cr, http.MethodGet, full, nil, out)
}

func do(ctx context.Context, cr connectorkit.Credentials, method, full string, body any, out any) error {
	if cr.AccessToken == "" {
		return fmt.Errorf("google: nobody's account is connected for this call")
	}

	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(raw)
	}

	req, err := http.NewRequestWithContext(ctx, method, full, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+cr.AccessToken)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	res, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(res.Body, 8<<20))

	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return apiError(res.StatusCode, raw, cr)
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(raw, out)
}

// apiError names the person, because "403 forbidden" on a per-user connector is
// ambiguous in the one way that matters: whose access failed.
func apiError(status int, raw []byte, cr connectorkit.Credentials) error {
	var body struct {
		Error struct {
			Message string `json:"message"`
			Status  string `json:"status"`
		} `json:"error"`
	}
	_ = json.Unmarshal(raw, &body)
	detail := strings.TrimSpace(body.Error.Message)
	if detail == "" {
		detail = snippet(raw)
	}

	who := cr.Account
	if who == "" {
		who = cr.Member
	}
	if who == "" {
		who = "the connected account"
	}

	switch status {
	case http.StatusUnauthorized:
		return fmt.Errorf("google: %s's sign-in is no longer valid — they need to reconnect: %s", who, detail)
	case http.StatusForbidden:
		return fmt.Errorf("google: %s's account is not permitted to do this (it may be an "+
			"admin restriction, or a scope they did not grant): %s", who, detail)
	case http.StatusNotFound:
		return fmt.Errorf("google: not found for %s: %s", who, detail)
	case http.StatusTooManyRequests:
		return fmt.Errorf("google: rate limited: %s", detail)
	default:
		return fmt.Errorf("google: request failed (%d) for %s: %s", status, who, detail)
	}
}

func (c *Connector) Tools() []connectorkit.Tool {
	return []connectorkit.Tool{
		{
			Name: "google.mail.search",
			Description: "Search the acting person's Gmail using Gmail query syntax, e.g. " +
				"`from:priya is:unread`, `subject:invoice newer_than:7d`. Returns headers and " +
				"a snippet, not full bodies — read one with google.mail.read.",
			Parameters: json.RawMessage(`{
				"type":"object",
				"properties":{
					"query":{"type":"string","description":"Gmail search query."},
					"limit":{"type":"integer","description":"default 10, max 50"}
				}
			}`),
			Call: mailSearch,
		},
		{
			Name:        "google.mail.read",
			Description: "Read one Gmail message in full, including its body.",
			Parameters: json.RawMessage(`{
				"type":"object",
				"properties":{"id":{"type":"string","description":"Message id from google.mail.search."}},
				"required":["id"]
			}`),
			Call: mailRead,
		},
		{
			Name: "google.mail.send",
			Description: "Send an email AS the acting person. This leaves their mailbox and " +
				"reaches a real recipient — propose it first unless they clearly asked you to send it.",
			Parameters: json.RawMessage(`{
				"type":"object",
				"properties":{
					"to":{"type":"string","description":"Recipient address. Comma-separate several."},
					"subject":{"type":"string"},
					"body":{"type":"string","description":"Plain text body."},
					"reply_to_id":{"type":"string","description":"Message id being replied to, to keep the thread."}
				},
				"required":["to","subject","body"]
			}`),
			Call: mailSend,
		},
		{
			Name: "google.calendar.list",
			Description: "List the acting person's upcoming calendar events. Use to answer what " +
				"they have on, or whether they are free.",
			Parameters: json.RawMessage(`{
				"type":"object",
				"properties":{
					"from":{"type":"string","description":"RFC3339 or YYYY-MM-DD. Defaults to now."},
					"to":{"type":"string","description":"RFC3339 or YYYY-MM-DD. Defaults to 7 days out."},
					"limit":{"type":"integer","description":"default 20, max 100"}
				}
			}`),
			Call: calendarList,
		},
		{
			Name: "google.calendar.create",
			Description: "Create a calendar event on the acting person's calendar. This is visible " +
				"to invitees immediately — propose it unless they asked for it directly.",
			Parameters: json.RawMessage(`{
				"type":"object",
				"properties":{
					"summary":{"type":"string","description":"Event title."},
					"start":{"type":"string","description":"RFC3339 start time."},
					"end":{"type":"string","description":"RFC3339 end time."},
					"description":{"type":"string"},
					"attendees":{"type":"string","description":"Comma-separated email addresses."}
				},
				"required":["summary","start","end"]
			}`),
			Call: calendarCreate,
		},
		{
			Name:        "google.drive.search",
			Description: "Search the acting person's Google Drive by name or content. Read-only.",
			Parameters: json.RawMessage(`{
				"type":"object",
				"properties":{
					"query":{"type":"string","description":"Words to look for in the name or contents."},
					"limit":{"type":"integer","description":"default 10, max 50"}
				},
				"required":["query"]
			}`),
			Call: driveSearch,
		},
		{
			Name: "google.drive.read",
			Description: "Read a Google Doc or Sheet as plain text, so its contents can be quoted " +
				"or summarised. Takes the id from google.drive.search.",
			Parameters: json.RawMessage(`{
				"type":"object",
				"properties":{"id":{"type":"string","description":"File id."}},
				"required":["id"]
			}`),
			Call: driveRead,
		},
		{
			Name:        "google.chat.spaces",
			Description: "List Google Chat spaces the acting person is in.",
			Parameters:  json.RawMessage(`{"type":"object","properties":{}}`),
			Call:        chatSpaces,
		},
		{
			Name:        "google.chat.post",
			Description: "Post a message to a Google Chat space as the acting person.",
			Parameters: json.RawMessage(`{
				"type":"object",
				"properties":{
					"space":{"type":"string","description":"Space name, e.g. spaces/AAAA."},
					"text":{"type":"string"}
				},
				"required":["space","text"]
			}`),
			Call: chatPost,
		},
	}
}

// --- Gmail -----------------------------------------------------------------

func mailSearch(ctx context.Context, cr connectorkit.Credentials, in map[string]any) (any, error) {
	limit := clamp(intArg(in, "limit", 10), 1, 50)
	q := url.Values{"maxResults": {fmt.Sprint(limit)}}
	if s := strArg(in, "query"); s != "" {
		q.Set("q", s)
	}

	var list struct {
		Messages []struct {
			ID string `json:"id"`
		} `json:"messages"`
	}
	if err := get(ctx, cr, gmailAPI+"/gmail/v1/users/me/messages", q, &list); err != nil {
		return nil, err
	}

	out := make([]map[string]any, 0, len(list.Messages))
	for _, m := range list.Messages {
		// metadata format: headers and a snippet without dragging every body
		// through the context window.
		var msg gmailMessage
		mq := url.Values{"format": {"metadata"}}
		mq["metadataHeaders"] = []string{"From", "To", "Subject", "Date"}
		if err := get(ctx, cr, gmailAPI+"/gmail/v1/users/me/messages/"+url.PathEscape(m.ID), mq, &msg); err != nil {
			continue
		}
		out = append(out, msg.summary())
	}
	return map[string]any{"count": len(out), "account": cr.Account, "messages": out}, nil
}

type gmailMessage struct {
	ID           string   `json:"id"`
	ThreadID     string   `json:"threadId"`
	Snippet      string   `json:"snippet"`
	LabelIDs     []string `json:"labelIds"`
	InternalDate string   `json:"internalDate"`
	Payload      struct {
		Headers []struct {
			Name  string `json:"name"`
			Value string `json:"value"`
		} `json:"headers"`
		MimeType string `json:"mimeType"`
		Body     struct {
			Data string `json:"data"`
		} `json:"body"`
		Parts []gmailPart `json:"parts"`
	} `json:"payload"`
}

type gmailPart struct {
	MimeType string `json:"mimeType"`
	Body     struct {
		Data string `json:"data"`
	} `json:"body"`
	Parts []gmailPart `json:"parts"`
}

func (m gmailMessage) header(name string) string {
	for _, h := range m.Payload.Headers {
		if strings.EqualFold(h.Name, name) {
			return h.Value
		}
	}
	return ""
}

func (m gmailMessage) summary() map[string]any {
	out := map[string]any{
		"id":      m.ID,
		"thread":  m.ThreadID,
		"from":    m.header("From"),
		"subject": m.header("Subject"),
		"snippet": strings.TrimSpace(m.Snippet),
	}
	if d := m.header("Date"); d != "" {
		out["date"] = d
	}
	for _, l := range m.LabelIDs {
		if l == "UNREAD" {
			out["unread"] = true
		}
	}
	return out
}

// body walks the MIME tree for text/plain, falling back to text/html.
//
// A Gmail message is a tree, not a string: a plain reply has its text at the
// top level, while anything with an attachment or rich formatting buries it
// several parts down. Reading only the top level returns "" for most real mail.
func (m gmailMessage) body() string {
	if txt := findPart(gmailPart{
		MimeType: m.Payload.MimeType,
		Body:     m.Payload.Body,
		Parts:    m.Payload.Parts,
	}, "text/plain"); txt != "" {
		return txt
	}
	return stripHTML(findPart(gmailPart{
		MimeType: m.Payload.MimeType,
		Body:     m.Payload.Body,
		Parts:    m.Payload.Parts,
	}, "text/html"))
}

func findPart(p gmailPart, want string) string {
	if strings.HasPrefix(p.MimeType, want) && p.Body.Data != "" {
		if dec, err := base64.URLEncoding.WithPadding(base64.NoPadding).DecodeString(p.Body.Data); err == nil {
			return string(dec)
		}
	}
	for _, sub := range p.Parts {
		if got := findPart(sub, want); got != "" {
			return got
		}
	}
	return ""
}

func mailRead(ctx context.Context, cr connectorkit.Credentials, in map[string]any) (any, error) {
	id := strArg(in, "id")
	if id == "" {
		return nil, fmt.Errorf("google: id is required")
	}
	var msg gmailMessage
	if err := get(ctx, cr, gmailAPI+"/gmail/v1/users/me/messages/"+url.PathEscape(id),
		url.Values{"format": {"full"}}, &msg); err != nil {
		return nil, err
	}
	out := msg.summary()
	out["to"] = msg.header("To")
	out["body"] = truncate(msg.body(), 8000)
	out["account"] = cr.Account
	return out, nil
}

func mailSend(ctx context.Context, cr connectorkit.Credentials, in map[string]any) (any, error) {
	to, subject, body := strArg(in, "to"), strArg(in, "subject"), strArg(in, "body")
	if to == "" || subject == "" || body == "" {
		return nil, fmt.Errorf("google: to, subject and body are all required")
	}

	var raw strings.Builder
	fmt.Fprintf(&raw, "To: %s\r\n", to)
	fmt.Fprintf(&raw, "Subject: %s\r\n", subject)
	raw.WriteString("MIME-Version: 1.0\r\n")
	raw.WriteString("Content-Type: text/plain; charset=UTF-8\r\n\r\n")
	raw.WriteString(body)

	payload := map[string]any{
		"raw": base64.URLEncoding.WithPadding(base64.NoPadding).EncodeToString([]byte(raw.String())),
	}
	// threadId keeps a reply in its conversation. Without it Gmail shows the
	// answer as a new thread, which reads to the recipient as a non sequitur.
	if t := strArg(in, "reply_to_id"); t != "" {
		var orig gmailMessage
		if err := get(ctx, cr, gmailAPI+"/gmail/v1/users/me/messages/"+url.PathEscape(t),
			url.Values{"format": {"metadata"}}, &orig); err == nil && orig.ThreadID != "" {
			payload["threadId"] = orig.ThreadID
		}
	}

	var res struct {
		ID       string `json:"id"`
		ThreadID string `json:"threadId"`
	}
	if err := do(ctx, cr, http.MethodPost, gmailAPI+"/gmail/v1/users/me/messages/send", payload, &res); err != nil {
		return nil, err
	}
	return map[string]any{"sent": true, "id": res.ID, "thread": res.ThreadID, "from": cr.Account}, nil
}

// --- Calendar ---------------------------------------------------------------

func calendarList(ctx context.Context, cr connectorkit.Credentials, in map[string]any) (any, error) {
	from, err := timeArg(in, "from", time.Now())
	if err != nil {
		return nil, err
	}
	to, err := timeArg(in, "to", from.AddDate(0, 0, 7))
	if err != nil {
		return nil, err
	}

	q := url.Values{
		"timeMin":      {from.Format(time.RFC3339)},
		"timeMax":      {to.Format(time.RFC3339)},
		"singleEvents": {"true"},
		"orderBy":      {"startTime"},
		"maxResults":   {fmt.Sprint(clamp(intArg(in, "limit", 20), 1, 100))},
	}

	var res struct {
		Items []struct {
			Summary string `json:"summary"`
			Start   struct {
				DateTime string `json:"dateTime"`
				Date     string `json:"date"`
			} `json:"start"`
			End struct {
				DateTime string `json:"dateTime"`
				Date     string `json:"date"`
			} `json:"end"`
			Location  string `json:"location"`
			HTMLLink  string `json:"htmlLink"`
			Attendees []struct {
				Email          string `json:"email"`
				ResponseStatus string `json:"responseStatus"`
			} `json:"attendees"`
		} `json:"items"`
	}
	if err := get(ctx, cr, calendarAPI+"/calendars/primary/events", q, &res); err != nil {
		return nil, err
	}

	out := make([]map[string]any, 0, len(res.Items))
	for _, e := range res.Items {
		// An all-day event has `date` and no `dateTime`. Reporting "" for its
		// start would make a real event look malformed.
		start := e.Start.DateTime
		allDay := false
		if start == "" {
			start, allDay = e.Start.Date, true
		}
		end := e.End.DateTime
		if end == "" {
			end = e.End.Date
		}
		row := map[string]any{"summary": e.Summary, "start": start, "end": end}
		if allDay {
			row["all_day"] = true
		}
		if e.Location != "" {
			row["location"] = e.Location
		}
		if n := len(e.Attendees); n > 0 {
			row["attendees"] = n
		}
		out = append(out, row)
	}
	return map[string]any{"count": len(out), "account": cr.Account, "events": out}, nil
}

func calendarCreate(ctx context.Context, cr connectorkit.Credentials, in map[string]any) (any, error) {
	summary, start, end := strArg(in, "summary"), strArg(in, "start"), strArg(in, "end")
	if summary == "" || start == "" || end == "" {
		return nil, fmt.Errorf("google: summary, start and end are all required")
	}
	if _, err := time.Parse(time.RFC3339, start); err != nil {
		return nil, fmt.Errorf("google: start must be an RFC3339 timestamp")
	}
	if _, err := time.Parse(time.RFC3339, end); err != nil {
		return nil, fmt.Errorf("google: end must be an RFC3339 timestamp")
	}

	body := map[string]any{
		"summary": summary,
		"start":   map[string]any{"dateTime": start},
		"end":     map[string]any{"dateTime": end},
	}
	if d := strArg(in, "description"); d != "" {
		body["description"] = d
	}
	if a := strArg(in, "attendees"); a != "" {
		var list []map[string]any
		for _, email := range strings.Split(a, ",") {
			if e := strings.TrimSpace(email); e != "" {
				list = append(list, map[string]any{"email": e})
			}
		}
		body["attendees"] = list
	}

	var res struct {
		ID       string `json:"id"`
		HTMLLink string `json:"htmlLink"`
	}
	if err := do(ctx, cr, http.MethodPost, calendarAPI+"/calendars/primary/events", body, &res); err != nil {
		return nil, err
	}
	return map[string]any{"created": true, "id": res.ID, "link": res.HTMLLink, "on": cr.Account}, nil
}

// --- Drive ------------------------------------------------------------------

func driveSearch(ctx context.Context, cr connectorkit.Credentials, in map[string]any) (any, error) {
	query := strArg(in, "query")
	if query == "" {
		return nil, fmt.Errorf("google: query is required")
	}
	// Escaping matters: an apostrophe in a filename otherwise terminates the
	// Drive query string and produces a syntax error rather than a search.
	esc := strings.ReplaceAll(query, `\`, `\\`)
	esc = strings.ReplaceAll(esc, `'`, `\'`)

	q := url.Values{
		"q":        {fmt.Sprintf("(name contains '%s' or fullText contains '%s') and trashed = false", esc, esc)},
		"pageSize": {fmt.Sprint(clamp(intArg(in, "limit", 10), 1, 50))},
		"fields":   {"files(id,name,mimeType,modifiedTime,webViewLink,owners(emailAddress))"},
	}

	var res struct {
		Files []struct {
			ID           string `json:"id"`
			Name         string `json:"name"`
			MimeType     string `json:"mimeType"`
			ModifiedTime string `json:"modifiedTime"`
			WebViewLink  string `json:"webViewLink"`
			Owners       []struct {
				EmailAddress string `json:"emailAddress"`
			} `json:"owners"`
		} `json:"files"`
	}
	if err := get(ctx, cr, driveAPI+"/files", q, &res); err != nil {
		return nil, err
	}

	out := make([]map[string]any, 0, len(res.Files))
	for _, f := range res.Files {
		row := map[string]any{"id": f.ID, "name": f.Name, "type": friendlyMime(f.MimeType),
			"modified": f.ModifiedTime, "link": f.WebViewLink}
		if len(f.Owners) > 0 {
			row["owner"] = f.Owners[0].EmailAddress
		}
		out = append(out, row)
	}
	return map[string]any{"count": len(out), "account": cr.Account, "files": out}, nil
}

func driveRead(ctx context.Context, cr connectorkit.Credentials, in map[string]any) (any, error) {
	id := strArg(in, "id")
	if id == "" {
		return nil, fmt.Errorf("google: id is required")
	}

	var meta struct {
		Name     string `json:"name"`
		MimeType string `json:"mimeType"`
	}
	if err := get(ctx, cr, driveAPI+"/files/"+url.PathEscape(id),
		url.Values{"fields": {"name,mimeType"}}, &meta); err != nil {
		return nil, err
	}

	// A Google-native file has no bytes to download; it must be EXPORTED to a
	// format. Everything else downloads directly. Getting this backwards
	// returns "Only files with binary content can be downloaded".
	var endpoint string
	q := url.Values{}
	if strings.HasPrefix(meta.MimeType, "application/vnd.google-apps.") {
		endpoint = driveAPI + "/files/" + url.PathEscape(id) + "/export"
		q.Set("mimeType", exportFormat(meta.MimeType))
	} else {
		endpoint = driveAPI + "/files/" + url.PathEscape(id)
		q.Set("alt", "media")
	}

	text, err := getText(ctx, cr, endpoint, q)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"id": id, "name": meta.Name, "type": friendlyMime(meta.MimeType),
		"account": cr.Account, "content": truncate(text, 12000),
	}, nil
}

func getText(ctx context.Context, cr connectorkit.Credentials, endpoint string, q url.Values) (string, error) {
	full := endpoint
	if len(q) > 0 {
		full += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, full, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+cr.AccessToken)

	res, err := httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(res.Body, 16<<20))
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return "", apiError(res.StatusCode, raw, cr)
	}
	return string(raw), nil
}

// exportFormat picks a plain-text export for a Google-native file.
func exportFormat(mime string) string {
	switch mime {
	case "application/vnd.google-apps.spreadsheet":
		return "text/csv"
	case "application/vnd.google-apps.presentation":
		return "text/plain"
	default:
		return "text/plain"
	}
}

func friendlyMime(mime string) string {
	switch mime {
	case "application/vnd.google-apps.document":
		return "doc"
	case "application/vnd.google-apps.spreadsheet":
		return "sheet"
	case "application/vnd.google-apps.presentation":
		return "slides"
	case "application/vnd.google-apps.folder":
		return "folder"
	case "application/pdf":
		return "pdf"
	default:
		return mime
	}
}

// --- Chat -------------------------------------------------------------------

func chatSpaces(ctx context.Context, cr connectorkit.Credentials, _ map[string]any) (any, error) {
	var res struct {
		Spaces []struct {
			Name        string `json:"name"`
			DisplayName string `json:"displayName"`
			Type        string `json:"spaceType"`
		} `json:"spaces"`
	}
	if err := get(ctx, cr, chatAPI+"/v1/spaces", url.Values{"pageSize": {"100"}}, &res); err != nil {
		return nil, err
	}
	out := make([]map[string]any, 0, len(res.Spaces))
	for _, s := range res.Spaces {
		out = append(out, map[string]any{"space": s.Name, "name": s.DisplayName, "type": s.Type})
	}
	return map[string]any{"count": len(out), "account": cr.Account, "spaces": out}, nil
}

func chatPost(ctx context.Context, cr connectorkit.Credentials, in map[string]any) (any, error) {
	space, text := strArg(in, "space"), strArg(in, "text")
	if space == "" || text == "" {
		return nil, fmt.Errorf("google: space and text are both required")
	}
	if !strings.HasPrefix(space, "spaces/") {
		space = "spaces/" + space
	}

	var res struct {
		Name string `json:"name"`
	}
	if err := do(ctx, cr, http.MethodPost, chatAPI+"/v1/"+space+"/messages",
		map[string]any{"text": text}, &res); err != nil {
		return nil, err
	}
	return map[string]any{"posted": true, "message": res.Name, "as": cr.Account}, nil
}
