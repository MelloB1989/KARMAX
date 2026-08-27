package jira

import (
	"encoding/json"
	"strings"
)

// personJSON is the shape Jira uses everywhere it names an account: assignee,
// reporter, comment author, changelog actor.
type personJSON struct {
	AccountID   string `json:"accountId"`
	DisplayName string `json:"displayName"`
}

func (p *personJSON) name() string {
	if p == nil {
		return ""
	}
	return p.DisplayName
}

func (p *personJSON) id() string {
	if p == nil {
		return ""
	}
	return p.AccountID
}

// issueJSON is one issue as returned by both /issue/{key} and /search.
type issueJSON struct {
	ID     string `json:"id"`
	Key    string `json:"key"`
	Self   string `json:"self"`
	Fields struct {
		Summary     string          `json:"summary"`
		Description json.RawMessage `json:"description"`
		Status      struct {
			Name string `json:"name"`
		} `json:"status"`
		Assignee *personJSON `json:"assignee"`
		Reporter *personJSON `json:"reporter"`
		Project  struct {
			Key  string `json:"key"`
			Name string `json:"name"`
		} `json:"project"`
		IssueType struct {
			Name string `json:"name"`
		} `json:"issuetype"`
		Created string `json:"created"`
		Updated string `json:"updated"`
		Comment struct {
			Total int `json:"total"`
		} `json:"comment"`
	} `json:"fields"`
	Changelog *changelogJSON `json:"changelog,omitempty"`
}

// changelogJSON is the delta a webhook delivery carries (Items only) or the
// full history a search with expand=changelog returns (Histories).
type changelogJSON struct {
	Items     []changelogItem    `json:"items,omitempty"`
	Histories []changelogHistory `json:"histories,omitempty"`
}

type changelogHistory struct {
	Created string          `json:"created"`
	Items   []changelogItem `json:"items"`
}

type changelogItem struct {
	Field      string `json:"field"`
	FromString string `json:"fromString"`
	ToString   string `json:"toString"`
}

// browseURL turns an API "self" link into the page a human would open —
// Jira's webhook and search payloads give you the REST link, never the
// browsable one.
func browseURL(self, key string) string {
	origin := self
	if i := strings.Index(self, "/rest/"); i >= 0 {
		origin = self[:i]
	}
	if origin == "" {
		return ""
	}
	return origin + "/browse/" + key
}

// statusDelta reads a webhook delta's changelog for the status transition it
// carries, if any. previous is "" when this delivery did not touch status.
func statusDelta(cl *changelogJSON) (previous string, changed []string) {
	if cl == nil {
		return "", nil
	}
	for _, it := range cl.Items {
		changed = append(changed, it.Field)
		if it.Field == "status" {
			previous = it.FromString
		}
	}
	return previous, changed
}

// assigneeDelta reads a webhook delta's changelog for an assignee change.
func assigneeDelta(cl *changelogJSON) (previous string, ok bool) {
	if cl == nil {
		return "", false
	}
	for _, it := range cl.Items {
		if it.Field == "assignee" {
			return it.FromString, true
		}
	}
	return "", false
}
