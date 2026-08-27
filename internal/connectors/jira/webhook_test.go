package jira

import "testing"

// The three payloads below follow Atlassian's own documented webhook shapes
// (developer.atlassian.com/cloud/jira/platform/webhooks) — issue and comment
// events share one envelope, distinguished by webhookEvent.

const issueCreatedDelivery = `{
	"timestamp": 1601613300000,
	"webhookEvent": "jira:issue_created",
	"issue_event_type_name": "issue_created",
	"user": {"accountId": "acc-1", "displayName": "Mia Krystof"},
	"issue": {
		"id": "10002", "key": "ED-1",
		"self": "https://acme.atlassian.net/rest/api/2/issue/10002",
		"fields": {
			"summary": "Optional cheese on the burger",
			"created": "2020-10-02T05:44:34.000+0000",
			"updated": "2020-10-02T05:44:34.000+0000",
			"project": {"id": "10000", "key": "ED", "name": "Edison Project"},
			"issuetype": {"id": "10001", "name": "Bug"},
			"status": {"id": "1", "name": "To Do"},
			"reporter": {"accountId": "acc-1", "displayName": "Mia Krystof"}
		}
	}
}`

const issueUpdatedDelivery = `{
	"timestamp": 1601613400000,
	"webhookEvent": "jira:issue_updated",
	"issue_event_type_name": "issue_generic",
	"user": {"accountId": "acc-2", "displayName": "Priya Singh"},
	"issue": {
		"id": "10002", "key": "ED-1",
		"self": "https://acme.atlassian.net/rest/api/2/issue/10002",
		"fields": {
			"summary": "Optional cheese on the burger",
			"created": "2020-10-02T05:44:34.000+0000",
			"updated": "2020-10-02T05:45:00.000+0000",
			"project": {"id": "10000", "key": "ED", "name": "Edison Project"},
			"issuetype": {"id": "10001", "name": "Bug"},
			"status": {"id": "3", "name": "In Progress"},
			"assignee": {"accountId": "acc-2", "displayName": "Priya Singh"},
			"reporter": {"accountId": "acc-1", "displayName": "Mia Krystof"}
		}
	},
	"changelog": {
		"id": "10124",
		"items": [
			{"field": "status", "fieldtype": "jira", "fieldId": "status",
			 "from": "1", "fromString": "To Do", "to": "3", "toString": "In Progress"}
		]
	}
}`

const commentCreatedDelivery = `{
	"timestamp": 1601613500000,
	"webhookEvent": "comment_created",
	"issue_event_type_name": "issue_commented",
	"user": {"accountId": "acc-2", "displayName": "Priya Singh"},
	"issue": {
		"id": "10002", "key": "ED-1",
		"self": "https://acme.atlassian.net/rest/api/2/issue/10002",
		"fields": {
			"summary": "Optional cheese on the burger",
			"project": {"id": "10000", "key": "ED", "name": "Edison Project"},
			"status": {"id": "3", "name": "In Progress"}
		}
	},
	"comment": {
		"self": "https://acme.atlassian.net/rest/api/2/issue/10002/comment/10000",
		"id": "10000",
		"author": {"accountId": "acc-2", "displayName": "Priya Singh"},
		"body": "Lets do it!",
		"created": "2020-10-02T05:45:34.000+0000"
	}
}`

func TestNoWebhookSecretMeansNoDeliveries(t *testing.T) {
	if verifyDelivery(creds(nil), map[string]string{"X-Jira-Webhook-Secret": "anything"}, []byte(`{}`)) {
		t.Error("a delivery was accepted with no secret configured")
	}
}

func TestVerifyDeliveryChecksTheSharedSecret(t *testing.T) {
	c := creds(map[string]string{"webhook_secret": "s3cret"})
	if verifyDelivery(c, map[string]string{}, []byte(`{}`)) {
		t.Error("a delivery with no header was accepted")
	}
	if verifyDelivery(c, map[string]string{"X-Jira-Webhook-Secret": "wrong"}, []byte(`{}`)) {
		t.Error("a delivery with the wrong secret was accepted")
	}
	if !verifyDelivery(c, map[string]string{"X-Jira-Webhook-Secret": "s3cret"}, []byte(`{}`)) {
		t.Error("a delivery with the right secret was refused")
	}
}

func TestDecodeIssueCreated(t *testing.T) {
	events, err := decodeIssueCreated(nil, []byte(issueCreatedDelivery))
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 {
		t.Fatalf("got %d events", len(events))
	}
	e := events[0]
	if e["key"] != "ED-1" || e["status"] != "To Do" || e["project"] != "ED" {
		t.Errorf("decoded %+v", e)
	}
	// No transition happened yet, so "previous" is honestly "current".
	if e["previous_status"] != "To Do" {
		t.Errorf("previous_status = %v, want the current status on a create", e["previous_status"])
	}
	if e["assignee"] != "" {
		t.Errorf("assignee = %v, want empty on an unassigned issue", e["assignee"])
	}

	// A delivery mounted on the wrong path — a comment or an update pointed at
	// the created source by a misconfigured automation rule — must not be
	// reshaped into a fake create.
	if got, err := decodeIssueCreated(nil, []byte(issueUpdatedDelivery)); err != nil || len(got) != 0 {
		t.Errorf("an update delivery on the created source produced %v (err %v)", got, err)
	}
}

func TestDecodeIssueUpdatedCarriesTheFieldsAnAwaitMatchesOn(t *testing.T) {
	events, err := decodeIssueUpdated(nil, []byte(issueUpdatedDelivery))
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 {
		t.Fatalf("got %d events", len(events))
	}
	e := events[0]
	for _, field := range []string{"key", "status", "previous_status", "assignee", "summary", "project"} {
		if _, ok := e[field]; !ok {
			t.Errorf("payload is missing required field %q: %+v", field, e)
		}
	}
	if e["key"] != "ED-1" || e["status"] != "In Progress" || e["previous_status"] != "To Do" {
		t.Errorf("status transition decoded wrong: %+v", e)
	}
	if e["assignee"] != "Priya Singh" {
		t.Errorf("assignee = %v", e["assignee"])
	}
	if changed, ok := e["changed_fields"].([]string); !ok || len(changed) != 1 || changed[0] != "status" {
		t.Errorf("changed_fields = %v", e["changed_fields"])
	}

	if got, err := decodeIssueUpdated(nil, []byte(issueCreatedDelivery)); err != nil || len(got) != 0 {
		t.Errorf("a created delivery on the updated source produced %v (err %v)", got, err)
	}
}

func TestDecodeIssueUpdatedWithNoStatusChangeKeepsPreviousEqualToCurrent(t *testing.T) {
	// Same delivery but with the changelog item retargeted at a field other
	// than status, e.g. an assignee-only edit.
	body := `{
		"webhookEvent": "jira:issue_updated",
		"issue": {"key": "ED-1", "fields": {"status": {"name": "In Progress"}, "summary": "x",
		          "project": {"key": "ED"}}},
		"changelog": {"items": [{"field": "assignee", "fromString": "Mia Krystof", "toString": "Priya Singh"}]}
	}`
	events, err := decodeIssueUpdated(nil, []byte(body))
	if err != nil || len(events) != 1 {
		t.Fatalf("events=%v err=%v", events, err)
	}
	e := events[0]
	if e["status"] != "In Progress" || e["previous_status"] != "In Progress" {
		t.Errorf("an assignee-only change should leave status unchanged: %+v", e)
	}
	if e["previous_assignee"] != "Mia Krystof" {
		t.Errorf("previous_assignee = %v", e["previous_assignee"])
	}
}

func TestDecodeCommentCreated(t *testing.T) {
	events, err := decodeCommentCreated(nil, []byte(commentCreatedDelivery))
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 {
		t.Fatalf("got %d events", len(events))
	}
	e := events[0]
	if e["key"] != "ED-1" || e["comment_id"] != "10000" || e["body"] != "Lets do it!" || e["author"] != "Priya Singh" {
		t.Errorf("decoded %+v", e)
	}
	if e["url"] != "https://acme.atlassian.net/browse/ED-1" {
		t.Errorf("url = %v", e["url"])
	}
}

func TestDecodeCommentCreatedHandlesAnADFBody(t *testing.T) {
	body := `{
		"webhookEvent": "comment_created",
		"issue": {"key": "ED-1", "self": "https://acme.atlassian.net/rest/api/2/issue/10002",
		          "fields": {"summary": "x", "status": {"name": "Open"}, "project": {"key": "ED"}}},
		"comment": {"id": "1", "author": {"displayName": "Mia"},
		            "body": {"type":"doc","version":1,"content":[{"type":"paragraph","content":[{"type":"text","text":"hi there"}]}]}}
	}`
	events, err := decodeCommentCreated(nil, []byte(body))
	if err != nil || len(events) != 1 {
		t.Fatalf("events=%v err=%v", events, err)
	}
	if events[0]["body"] != "hi there" {
		t.Errorf("body = %v", events[0]["body"])
	}
}

func TestUninterestingWebhookEventsProduceNothing(t *testing.T) {
	body := []byte(`{"webhookEvent": "jira:issue_deleted", "issue": {"key": "ED-2"}}`)
	for _, decode := range []func(map[string]string, []byte) ([]map[string]any, error){
		decodeIssueCreated, decodeIssueUpdated, decodeCommentCreated,
	} {
		got, err := decode(nil, body)
		if err != nil || len(got) != 0 {
			t.Errorf("an uninteresting delivery produced %v (err %v)", got, err)
		}
	}
}

func TestUndecodableDeliveryIsAnError(t *testing.T) {
	if _, err := decodeIssueUpdated(nil, []byte(`not json`)); err == nil {
		t.Error("malformed JSON was accepted")
	}
}
