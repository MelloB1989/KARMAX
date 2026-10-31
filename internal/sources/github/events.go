// Package github is the GitHub event source: every account the gh CLI is
// logged in to, polled for notifications, each new or updated thread turned
// into a sources.Event on that account.
//
// Notifications rather than webhooks, because they need no public endpoint
// and already say why this account was told: a review requested of it, a
// mention, a failing run it started. Nothing here marks a thread read — the
// inbox is the operator's.
package github

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/MelloB1989/karmax/internal/safety"
	"github.com/MelloB1989/karmax/internal/sources"
)

// Notification is one thread from GET /notifications.
type Notification struct {
	ID        string    `json:"id"`
	Unread    bool      `json:"unread"`
	Reason    string    `json:"reason"`
	UpdatedAt time.Time `json:"updated_at"`
	Subject   struct {
		Title            string `json:"title"`
		URL              string `json:"url"`
		LatestCommentURL string `json:"latest_comment_url"`
		Type             string `json:"type"`
	} `json:"subject"`
	Repository struct {
		FullName string `json:"full_name"`
		HTMLURL  string `json:"html_url"`
		Private  bool   `json:"private"`
		Owner    struct {
			Login string `json:"login"`
		} `json:"owner"`
	} `json:"repository"`
}

// Key is the thread at this moment: a thread with new activity is a new key.
func (n Notification) Key() string {
	return n.ID + "@" + n.UpdatedAt.UTC().Format(time.RFC3339)
}

// Detail is what reading the subject and its latest comment added. The zero
// value is an event built from the notification alone.
type Detail struct {
	HTMLURL string
	State   string // open, closed, merged, draft
	Author  string
	Number  int

	CommentAuthor string
	CommentBody   string
	CommentURL    string
}

// urgentReasons are the reasons a person is waiting on the operator.
var urgentReasons = map[string]bool{
	"review_requested":   true,
	"mention":            true,
	"assign":             true,
	"security_alert":     true,
	"approval_requested": true, // a deployment is held until they approve it
}

// runFailed reads a ci_activity title. GitHub only sends these for runs the
// account itself started, so a failing one is the operator's own work.
var runFailed = regexp.MustCompile(`(?i)\b(fail(ed|ing|ure|s)?|errored|timed out)\b`)

// Urgent reports whether the operator must hear about this now: someone needs
// them directly, or their own run failed. A thread already read on GitHub is
// one they have seen, so it never interrupts them again.
func Urgent(n Notification) bool {
	if !n.Unread {
		return false
	}
	if urgentReasons[n.Reason] {
		return true
	}
	return n.Reason == "ci_activity" && runFailed.MatchString(n.Subject.Title)
}

// Event turns a thread into what every source publishes.
func Event(account string, n Notification, d Detail) sources.Event {
	number := d.Number
	if number == 0 {
		number = numberFrom(n.Subject.URL)
	}
	repo := n.Repository.FullName
	ref := repo
	if number > 0 {
		ref = repo + "#" + strconv.Itoa(number)
	}
	subject := oneLine(n.Subject.Title, 140)
	url := d.HTMLURL
	if url == "" {
		url = htmlURL(n)
	}

	data := map[string]any{
		"repo":          repo,
		"owner":         n.Repository.Owner.Login,
		"subject_type":  n.Subject.Type,
		"subject_title": subject,
		"reason":        n.Reason,
		"unread":        n.Unread,
		"thread_id":     n.ID,
		"updated_at":    n.UpdatedAt.UTC().Format(time.RFC3339),
		"private":       n.Repository.Private,
	}
	if number > 0 {
		data["number"] = number
	}
	for k, v := range map[string]string{"state": d.State, "author": d.Author,
		"comment_author": d.CommentAuthor, "comment_url": d.CommentURL} {
		if v != "" {
			data[k] = v
		}
	}

	return sources.Event{
		Source:  "github",
		Account: account,
		Type:    n.Reason + "." + n.Subject.Type,
		Key:     n.Key(),
		Title:   fmt.Sprintf("%s: %s (%s)", headline(n), subject, ref),
		Summary: summary(account, n, d, ref),
		URL:     url,
		At:      n.UpdatedAt,
		Urgent:  Urgent(n),
		Data:    data,
	}
}

func headline(n Notification) string {
	switch n.Reason {
	case "review_requested":
		return "Review requested"
	case "mention":
		return "Mentioned"
	case "team_mention":
		return "Team mentioned"
	case "assign":
		return "Assigned"
	case "security_alert":
		return "Security alert"
	case "approval_requested":
		return "Deployment approval requested"
	case "ci_activity":
		return "CI"
	case "author":
		return "Activity on your " + kind(n.Subject.Type)
	case "comment":
		return "New comment"
	case "state_change":
		return "State changed"
	case "invitation":
		return "Invitation"
	}
	return "GitHub"
}

// why says, for the summary, why this account was notified.
var why = map[string]string{
	"review_requested":         "a review was requested of it",
	"mention":                  "it was @mentioned",
	"team_mention":             "a team it is on was mentioned",
	"assign":                   "it was assigned",
	"security_alert":           "a security alert fired on a repository it maintains",
	"approval_requested":       "a deployment is waiting for its approval",
	"ci_activity":              "a workflow run it started finished",
	"author":                   "it opened the thread",
	"comment":                  "it commented on the thread",
	"state_change":             "it changed the thread's state",
	"subscribed":               "it watches the repository",
	"manual":                   "it subscribed to the thread",
	"invitation":               "it was invited to the repository",
	"security_advisory_credit": "it was credited on a security advisory",
	"member_feature_requested": "a member asked for a feature",
}

func summary(account string, n Notification, d Detail, ref string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s, a %s", ref, kind(n.Subject.Type))
	if d.State != "" {
		fmt.Fprintf(&b, " (%s)", d.State)
	}
	if d.Author != "" {
		fmt.Fprintf(&b, " by %s", d.Author)
	}
	reason := why[n.Reason]
	if reason == "" {
		reason = "of " + strings.ReplaceAll(n.Reason, "_", " ")
	}
	fmt.Fprintf(&b, ". GitHub told %s because %s", account, reason)
	if !n.Unread {
		b.WriteString("; already read on GitHub")
	}
	b.WriteString(".")

	// The title and the comment were written by whoever opened the thread or
	// replied in it — on a public repository, anyone.
	text := "Title: " + n.Subject.Title
	if body := strings.TrimSpace(d.CommentBody); body != "" {
		who := d.CommentAuthor
		if who == "" {
			who = "someone"
		}
		text += "\n\nLatest comment, by " + who + ":\n" + excerpt(body, 700)
	}
	b.WriteString("\n")
	b.WriteString(safety.Fence("GitHub "+ref+", written by people other than the operator", text))
	return b.String()
}

// kind names a subject type the way a person would.
func kind(t string) string {
	switch t {
	case "PullRequest":
		return "pull request"
	case "Issue":
		return "issue"
	case "CheckSuite", "WorkflowRun":
		return "workflow run"
	case "Commit":
		return "commit"
	case "Release":
		return "release"
	case "Discussion":
		return "discussion"
	case "RepositoryVulnerabilityAlert", "RepositoryDependabotAlertsThread":
		return "security alert"
	case "RepositoryInvitation":
		return "repository invitation"
	case "":
		return "thread"
	}
	return t
}

// numberFrom reads the issue or pull request number off an API URL.
func numberFrom(apiURL string) int {
	parts := strings.Split(strings.TrimSuffix(apiURL, "/"), "/")
	if len(parts) < 2 {
		return 0
	}
	switch parts[len(parts)-2] {
	case "pulls", "issues":
		n, _ := strconv.Atoi(parts[len(parts)-1])
		return n
	}
	return 0
}

// htmlURL is the page to open when the subject itself was not read.
func htmlURL(n Notification) string {
	repo := strings.TrimSuffix(n.Repository.HTMLURL, "/")
	if i := strings.Index(n.Subject.URL, "/repos/"+n.Repository.FullName+"/"); i >= 0 && repo != "" {
		rest := n.Subject.URL[i+len("/repos/"+n.Repository.FullName+"/"):]
		seg, id, _ := strings.Cut(rest, "/")
		switch seg {
		case "pulls":
			return repo + "/pull/" + id
		case "issues":
			return repo + "/issues/" + id
		case "commits":
			return repo + "/commit/" + id
		}
	}
	switch n.Subject.Type {
	case "CheckSuite", "WorkflowRun":
		return repo + "/actions"
	case "Discussion":
		return repo + "/discussions"
	case "RepositoryVulnerabilityAlert", "RepositoryDependabotAlertsThread":
		return repo + "/security"
	case "Release":
		return repo + "/releases"
	}
	return repo
}

// oneLine is text a person reads on one line: no breaks, no fence
// delimiters, no longer than max runes.
func oneLine(s string, max int) string {
	s = strings.Join(strings.Fields(safety.Defang(s)), " ")
	return truncate(s, max)
}

func excerpt(s string, max int) string { return truncate(strings.TrimSpace(s), max) }

func truncate(s string, max int) string {
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	r := []rune(s)
	return strings.TrimSpace(string(r[:max])) + "…"
}
