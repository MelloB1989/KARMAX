package github

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/MelloB1989/karmax/pkg/connectorkit"
)

// verifyDelivery checks GitHub's HMAC over the raw body.
//
// Refused when no secret is configured rather than accepted: an unverified
// webhook is an open endpoint that anything can post events into, and those
// events trigger loops.
func verifyDelivery(cr connectorkit.Credentials, headers map[string]string, body []byte) bool {
	secret := cr.Get("webhook_secret")
	if secret == "" {
		return false
	}
	sig := strings.TrimPrefix(header(headers, "X-Hub-Signature-256"), "sha256=")
	if sig == "" {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hmac.Equal([]byte(hex.EncodeToString(mac.Sum(nil))), []byte(sig))
}

// decodeDelivery turns a delivery into at most one event.
//
// Most deliveries are not interesting, and returning nothing is the normal
// case — a connector that publishes every webhook it receives makes the event
// log useless for the ones that matter.
func decodeDelivery(headers map[string]string, body []byte) ([]map[string]any, error) {
	switch event := header(headers, "X-GitHub-Event"); event {
	case "issues", "issue_comment":
		return decodeIssueDelivery(event, body)
	case "pull_request":
		return decodePullRequestDelivery(body)
	case "pull_request_review":
		return decodePullRequestReviewDelivery(body)
	case "check_suite":
		return decodeCheckSuiteDelivery(body)
	default:
		return nil, nil
	}
}

func decodeIssueDelivery(event string, body []byte) ([]map[string]any, error) {
	var d struct {
		Action     string `json:"action"`
		Repository struct {
			FullName string `json:"full_name"`
		} `json:"repository"`
		Sender struct {
			Login string `json:"login"`
		} `json:"sender"`
		Issue struct {
			Number int    `json:"number"`
			Title  string `json:"title"`
			Body   string `json:"body"`
			URL    string `json:"html_url"`
		} `json:"issue"`
		Comment struct {
			Body string `json:"body"`
			URL  string `json:"html_url"`
		} `json:"comment"`
	}
	if err := json.Unmarshal(body, &d); err != nil {
		return nil, fmt.Errorf("github: undecodable %s delivery: %w", event, err)
	}

	text, url := d.Issue.Body, d.Issue.URL
	if d.Comment.Body != "" {
		text, url = d.Comment.Body, d.Comment.URL
	}
	return []map[string]any{{
		"event": event, "action": d.Action, "repo": d.Repository.FullName,
		"number": d.Issue.Number, "title": d.Issue.Title, "actor": d.Sender.Login,
		"url": url,
		// Written by whoever opened the issue or left the comment, so it is
		// fenced by the host (isFreeText covers "title" and "body") before it
		// reaches a prompt.
		"body": text,
	}}, nil
}

// decodePullRequestDelivery covers every pull_request action, matching the
// old behaviour for anything not opened/closed, and adds "kind" for the three
// this connector treats as durable events.
func decodePullRequestDelivery(body []byte) ([]map[string]any, error) {
	var d struct {
		Action     string `json:"action"`
		Repository struct {
			FullName string `json:"full_name"`
		} `json:"repository"`
		Sender struct {
			Login string `json:"login"`
		} `json:"sender"`
		PullRequest struct {
			Number  int    `json:"number"`
			Title   string `json:"title"`
			Body    string `json:"body"`
			HTMLURL string `json:"html_url"`
			State   string `json:"state"`
			Draft   bool   `json:"draft"`
			Merged  bool   `json:"merged"`
			Head    struct {
				Ref string `json:"ref"`
			} `json:"head"`
			Base struct {
				Ref string `json:"ref"`
			} `json:"base"`
			User struct {
				Login string `json:"login"`
			} `json:"user"`
			MergedBy struct {
				Login string `json:"login"`
			} `json:"merged_by"`
		} `json:"pull_request"`
	}
	if err := json.Unmarshal(body, &d); err != nil {
		return nil, fmt.Errorf("github: undecodable pull_request delivery: %w", err)
	}
	pr := d.PullRequest

	out := map[string]any{
		"event": "pull_request", "action": d.Action, "repo": d.Repository.FullName,
		"number": pr.Number, "title": pr.Title, "actor": d.Sender.Login,
		"url": pr.HTMLURL, "body": pr.Body, "draft": pr.Draft,
		"branch": pr.Head.Ref, "base": pr.Base.Ref, "author": pr.User.Login,
		"state": pr.State,
	}
	switch {
	case d.Action == "opened":
		out["kind"] = "github.pr.opened"
	case d.Action == "closed" && pr.Merged:
		out["kind"] = "github.pr.merged"
		out["merged_by"] = pr.MergedBy.Login
	case d.Action == "closed":
		out["kind"] = "github.pr.closed"
	}
	return []map[string]any{out}, nil
}

// decodePullRequestReviewDelivery surfaces the review itself — its state and
// its comment — rather than the PR's own body, which the old shared decoder
// returned for this event type.
func decodePullRequestReviewDelivery(body []byte) ([]map[string]any, error) {
	var d struct {
		Action     string `json:"action"`
		Repository struct {
			FullName string `json:"full_name"`
		} `json:"repository"`
		Sender struct {
			Login string `json:"login"`
		} `json:"sender"`
		PullRequest struct {
			Number int    `json:"number"`
			Title  string `json:"title"`
			State  string `json:"state"`
			Head   struct {
				Ref string `json:"ref"`
			} `json:"head"`
		} `json:"pull_request"`
		Review struct {
			State   string `json:"state"`
			Body    string `json:"body"`
			HTMLURL string `json:"html_url"`
			User    struct {
				Login string `json:"login"`
			} `json:"user"`
		} `json:"review"`
	}
	if err := json.Unmarshal(body, &d); err != nil {
		return nil, fmt.Errorf("github: undecodable pull_request_review delivery: %w", err)
	}

	out := map[string]any{
		"event": "pull_request_review", "action": d.Action, "repo": d.Repository.FullName,
		"number": d.PullRequest.Number, "branch": d.PullRequest.Head.Ref, "title": d.PullRequest.Title,
		"actor": d.Sender.Login, "author": d.Review.User.Login,
		"state": d.PullRequest.State, "review_state": d.Review.State,
		"url": d.Review.HTMLURL, "body": d.Review.Body,
	}
	if d.Action == "submitted" {
		out["kind"] = "github.pr.review_submitted"
	}
	return []map[string]any{out}, nil
}

// decodeCheckSuiteDelivery keeps only "completed" — queued and in_progress
// are noise a recipe never wants to wake up for.
func decodeCheckSuiteDelivery(body []byte) ([]map[string]any, error) {
	var d struct {
		Action     string `json:"action"`
		Repository struct {
			FullName string `json:"full_name"`
		} `json:"repository"`
		Sender struct {
			Login string `json:"login"`
		} `json:"sender"`
		CheckSuite struct {
			HeadBranch string `json:"head_branch"`
			HeadSHA    string `json:"head_sha"`
			Conclusion string `json:"conclusion"`
			URL        string `json:"url"`
			App        struct {
				Name string `json:"name"`
			} `json:"app"`
			PullRequests []struct {
				Number int `json:"number"`
			} `json:"pull_requests"`
		} `json:"check_suite"`
	}
	if err := json.Unmarshal(body, &d); err != nil {
		return nil, fmt.Errorf("github: undecodable check_suite delivery: %w", err)
	}
	if d.Action != "completed" {
		return nil, nil
	}

	number := 0
	if len(d.CheckSuite.PullRequests) > 0 {
		number = d.CheckSuite.PullRequests[0].Number
	}
	title := d.CheckSuite.App.Name
	if title == "" {
		title = "check suite"
	}
	return []map[string]any{{
		"kind": "github.check_suite.completed", "event": "check_suite", "action": d.Action,
		"repo": d.Repository.FullName, "number": number, "branch": d.CheckSuite.HeadBranch,
		"title": title, "actor": d.Sender.Login, "author": d.Sender.Login,
		"state": d.CheckSuite.Conclusion, "url": d.CheckSuite.URL, "sha": d.CheckSuite.HeadSHA,
	}}, nil
}
