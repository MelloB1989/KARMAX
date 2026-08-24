package jira

import (
	"crypto/subtle"
	"encoding/json"
	"fmt"

	"github.com/MelloB1989/karmax/pkg/connectorkit"
)

// Event kinds this connector publishes. Other packs write recipes against
// these exact strings, so they are not renamed lightly.
const (
	EventIssueCreated   = "jira.issue.created"
	EventIssueUpdated   = "jira.issue.updated"
	EventCommentCreated = "jira.comment.created"
)

// webhookDelivery is the shape of a Jira Cloud webhook POST — issue and
// comment events share one envelope, distinguished by webhookEvent.
type webhookDelivery struct {
	WebhookEvent       string         `json:"webhookEvent"`
	IssueEventTypeName string         `json:"issue_event_type_name"`
	User               personJSON     `json:"user"`
	Issue              issueJSON      `json:"issue"`
	Changelog          *changelogJSON `json:"changelog"`
	Comment            *struct {
		ID     string          `json:"id"`
		Self   string          `json:"self"`
		Body   json.RawMessage `json:"body"`
		Author personJSON      `json:"author"`
	} `json:"comment"`
}

// verifyDelivery checks a shared secret carried in a header.
//
// Jira has no HMAC signing for webhooks the way GitHub does: native System
// WebHooks send a bare POST, and only Jira Automation's "Send web request"
// action lets you attach a custom header at all. So the shared secret rides in
// X-Jira-Webhook-Secret, set on that action, and a delivery with no secret
// configured is refused rather than accepted — an unverified endpoint is open
// to anyone who finds the URL, and those events trigger loops.
func verifyDelivery(cr connectorkit.Credentials, headers map[string]string, body []byte) bool {
	secret := cr.Get("webhook_secret")
	if secret == "" {
		return false
	}
	got := header(headers, "X-Jira-Webhook-Secret")
	if got == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(secret)) == 1
}

// decodeIssueCreated decodes a delivery mounted on the issue-created source.
// Anything but a genuine jira:issue_created is refused rather than silently
// reshaped, since a misrouted delivery (wrong URL pasted into the wrong Jira
// automation rule) must not masquerade as the wrong event kind.
func decodeIssueCreated(headers map[string]string, body []byte) ([]map[string]any, error) {
	d, err := unmarshalDelivery(body)
	if err != nil {
		return nil, err
	}
	if d.WebhookEvent != "jira:issue_created" {
		return nil, nil
	}
	return []map[string]any{issuePayload(d, "", "", nil)}, nil
}

func decodeIssueUpdated(headers map[string]string, body []byte) ([]map[string]any, error) {
	d, err := unmarshalDelivery(body)
	if err != nil {
		return nil, err
	}
	if d.WebhookEvent != "jira:issue_updated" {
		return nil, nil
	}
	prevStatus, changed := statusDelta(d.Changelog)
	prevAssignee, _ := assigneeDelta(d.Changelog)
	return []map[string]any{issuePayload(d, prevStatus, prevAssignee, changed)}, nil
}

func decodeCommentCreated(headers map[string]string, body []byte) ([]map[string]any, error) {
	d, err := unmarshalDelivery(body)
	if err != nil {
		return nil, err
	}
	if d.WebhookEvent != "comment_created" || d.Comment == nil {
		return nil, nil
	}
	return []map[string]any{{
		"key":        d.Issue.Key,
		"comment_id": d.Comment.ID,
		"body":       adfText(d.Comment.Body),
		"author":     d.Comment.Author.name(),
		"status":     d.Issue.Fields.Status.Name,
		"project":    d.Issue.Fields.Project.Key,
		"summary":    d.Issue.Fields.Summary,
		"url":        browseURL(d.Comment.Self, d.Issue.Key),
	}}, nil
}

func unmarshalDelivery(body []byte) (webhookDelivery, error) {
	var d webhookDelivery
	if err := json.Unmarshal(body, &d); err != nil {
		return d, fmt.Errorf("jira: undecodable webhook delivery: %w", err)
	}
	return d, nil
}

// issuePayload builds the fields a jira.issue.* event carries. prevStatus and
// prevAssignee are "" when this delivery did not change that field, in which
// case the current value is used — nothing transitioned, so "previous" and
// "current" are honestly the same thing.
func issuePayload(d webhookDelivery, prevStatus, prevAssignee string, changed []string) map[string]any {
	status := d.Issue.Fields.Status.Name
	assignee := d.Issue.Fields.Assignee.name()
	if prevStatus == "" {
		prevStatus = status
	}
	if prevAssignee == "" {
		prevAssignee = assignee
	}
	return map[string]any{
		"key":               d.Issue.Key,
		"summary":           d.Issue.Fields.Summary,
		"status":            status,
		"previous_status":   prevStatus,
		"assignee":          assignee,
		"previous_assignee": prevAssignee,
		"reporter":          d.Issue.Fields.Reporter.name(),
		"project":           d.Issue.Fields.Project.Key,
		"project_name":      d.Issue.Fields.Project.Name,
		"issue_type":        d.Issue.Fields.IssueType.Name,
		"url":               browseURL(d.Issue.Self, d.Issue.Key),
		"actor":             d.User.name(),
		"changed_fields":    changed,
	}
}
