package orgops

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// GitHub is the slice of the REST API the office uses. The base URL is
// configurable, which is what lets GitHub Enterprise and the bundled fake
// server (cmd/fakegithub) stand in for github.com without a code path of their own.
type GitHub struct {
	base  string
	token string
	http  *http.Client
}

// NewGitHub returns a client for base ("https://api.github.com").
func NewGitHub(base, token string) *GitHub {
	return &GitHub{
		base:  strings.TrimRight(base, "/"),
		token: token,
		http:  &http.Client{Timeout: 30 * time.Second},
	}
}

// ErrNotFound is a 404 — a missing file or PR, not a broken API.
var ErrNotFound = errors.New("github: not found")

// APIError is any other non-2xx answer.
type APIError struct {
	Status int
	Body   string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("github: HTTP %d: %s", e.Status, truncate(e.Body, 300))
}

func (g *GitHub) do(ctx context.Context, method, p string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, g.base+p, body)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if g.token != "" {
		req.Header.Set("Authorization", "Bearer "+g.token)
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := g.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode == http.StatusNotFound {
		return ErrNotFound
	}
	if resp.StatusCode/100 != 2 {
		return &APIError{Status: resp.StatusCode, Body: string(raw)}
	}
	if out == nil || len(raw) == 0 {
		return nil
	}
	return json.Unmarshal(raw, out)
}

// PR is a pull request, as much of it as a review needs.
type PR struct {
	Number       int    `json:"number"`
	Title        string `json:"title"`
	Body         string `json:"body"`
	State        string `json:"state"`
	Draft        bool   `json:"draft"`
	Merged       bool   `json:"merged"`
	HTMLURL      string `json:"html_url"`
	Additions    int    `json:"additions"`
	Deletions    int    `json:"deletions"`
	ChangedFiles int    `json:"changed_files"`
	User         struct {
		Login string `json:"login"`
	} `json:"user"`
	Head struct {
		Ref string `json:"ref"`
		SHA string `json:"sha"`
	} `json:"head"`
	Base struct {
		Ref string `json:"ref"`
	} `json:"base"`
}

// PRFile is one file a PR changes. Patch is the unified diff hunk text.
type PRFile struct {
	Filename  string `json:"filename"`
	Status    string `json:"status"`
	Additions int    `json:"additions"`
	Deletions int    `json:"deletions"`
	Patch     string `json:"patch"`
}

// ReviewComment is an inline comment on a review.
type ReviewComment struct {
	Path string `json:"path"`
	Line int    `json:"line,omitempty"`
	Side string `json:"side,omitempty"`
	Body string `json:"body"`
}

// Repo is a repository's identity.
type Repo struct {
	Name          string `json:"name"`
	FullName      string `json:"full_name"`
	Description   string `json:"description"`
	DefaultBranch string `json:"default_branch"`
	HTMLURL       string `json:"html_url"`
	Private       bool   `json:"private"`
	Archived      bool   `json:"archived"`
}

func (g *GitHub) GetPR(ctx context.Context, repo string, n int) (PR, error) {
	var pr PR
	err := g.do(ctx, "GET", fmt.Sprintf("/repos/%s/pulls/%d", repo, n), nil, &pr)
	return pr, err
}

// ListPRFiles returns every file the PR touches (first 300, which is GitHub's own cap).
func (g *GitHub) ListPRFiles(ctx context.Context, repo string, n int) ([]PRFile, error) {
	var all []PRFile
	for page := 1; page <= 3; page++ {
		var files []PRFile
		if err := g.do(ctx, "GET", fmt.Sprintf("/repos/%s/pulls/%d/files?per_page=100&page=%d", repo, n, page), nil, &files); err != nil {
			return all, err
		}
		all = append(all, files...)
		if len(files) < 100 {
			break
		}
	}
	return all, nil
}

// FindPRByHead finds the open PR whose head branch is branch.
func (g *GitHub) FindPRByHead(ctx context.Context, repo, branch string) (PR, bool, error) {
	owner := strings.SplitN(repo, "/", 2)[0]
	var prs []PR
	q := url.Values{"state": {"open"}, "head": {owner + ":" + branch}}
	if err := g.do(ctx, "GET", fmt.Sprintf("/repos/%s/pulls?%s", repo, q.Encode()), nil, &prs); err != nil {
		return PR{}, false, err
	}
	for _, pr := range prs {
		if pr.Head.Ref == branch {
			return pr, true, nil
		}
	}
	return PR{}, false, nil
}

// CreateReview posts a review. event is APPROVE, REQUEST_CHANGES or COMMENT.
func (g *GitHub) CreateReview(ctx context.Context, repo string, n int, event, body string, comments []ReviewComment) error {
	in := map[string]any{"event": event, "body": body}
	if len(comments) > 0 {
		in["comments"] = comments
	}
	err := g.do(ctx, "POST", fmt.Sprintf("/repos/%s/pulls/%d/reviews", repo, n), in, nil)
	var apiErr *APIError
	// Inline comments on lines GitHub cannot place reject the whole review;
	// the summary is what matters, so it goes out without them.
	if errors.As(err, &apiErr) && apiErr.Status == http.StatusUnprocessableEntity && len(comments) > 0 {
		return g.do(ctx, "POST", fmt.Sprintf("/repos/%s/pulls/%d/reviews", repo, n),
			map[string]any{"event": event, "body": body}, nil)
	}
	return err
}

// Comment posts a conversation comment on a PR or issue.
func (g *GitHub) Comment(ctx context.Context, repo string, n int, body string) error {
	return g.do(ctx, "POST", fmt.Sprintf("/repos/%s/issues/%d/comments", repo, n), map[string]any{"body": body}, nil)
}

// Merge merges a PR. method is merge, squash or rebase.
func (g *GitHub) Merge(ctx context.Context, repo string, n int, method, title string) error {
	if method == "" {
		method = "squash"
	}
	in := map[string]any{"merge_method": method}
	if title != "" {
		in["commit_title"] = title
	}
	return g.do(ctx, "PUT", fmt.Sprintf("/repos/%s/pulls/%d/merge", repo, n), in, nil)
}

// CIState folds commit statuses and check runs into one word:
// success, pending, failure, or none when nothing reports on the commit.
func (g *GitHub) CIState(ctx context.Context, repo, ref string) (string, error) {
	var combined struct {
		State    string `json:"state"`
		Statuses []struct {
			State string `json:"state"`
		} `json:"statuses"`
	}
	if err := g.do(ctx, "GET", fmt.Sprintf("/repos/%s/commits/%s/status", repo, ref), nil, &combined); err != nil && !errors.Is(err, ErrNotFound) {
		return "", err
	}
	var checks struct {
		CheckRuns []struct {
			Status     string `json:"status"`
			Conclusion string `json:"conclusion"`
		} `json:"check_runs"`
	}
	if err := g.do(ctx, "GET", fmt.Sprintf("/repos/%s/commits/%s/check-runs", repo, ref), nil, &checks); err != nil && !errors.Is(err, ErrNotFound) {
		return "", err
	}
	seen, pending, failed := 0, false, false
	if len(combined.Statuses) > 0 {
		seen++
		switch combined.State {
		case "failure", "error":
			failed = true
		case "pending":
			pending = true
		}
	}
	for _, r := range checks.CheckRuns {
		seen++
		if r.Status != "completed" {
			pending = true
			continue
		}
		switch r.Conclusion {
		case "success", "neutral", "skipped":
		default:
			failed = true
		}
	}
	switch {
	case failed:
		return "failure", nil
	case pending:
		return "pending", nil
	case seen == 0:
		return "none", nil
	}
	return "success", nil
}

// GetFile reads a file's text at ref (the default branch when ref is empty).
func (g *GitHub) GetFile(ctx context.Context, repo, p, ref string) (string, error) {
	u := fmt.Sprintf("/repos/%s/contents/%s", repo, strings.TrimLeft(p, "/"))
	if ref != "" {
		u += "?ref=" + url.QueryEscape(ref)
	}
	var f struct {
		Content  string `json:"content"`
		Encoding string `json:"encoding"`
		Type     string `json:"type"`
	}
	if err := g.do(ctx, "GET", u, nil, &f); err != nil {
		return "", err
	}
	if f.Type != "" && f.Type != "file" {
		return "", ErrNotFound
	}
	if f.Encoding == "base64" {
		b, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(f.Content, "\n", ""))
		if err != nil {
			return "", err
		}
		return string(b), nil
	}
	return f.Content, nil
}

// PutFile creates or replaces a file on a branch.
func (g *GitHub) PutFile(ctx context.Context, repo, p, content, message, branch string) error {
	in := map[string]any{
		"message": message,
		"content": base64.StdEncoding.EncodeToString([]byte(content)),
	}
	if branch != "" {
		in["branch"] = branch
	}
	u := fmt.Sprintf("/repos/%s/contents/%s", repo, strings.TrimLeft(p, "/"))
	var existing struct {
		SHA string `json:"sha"`
	}
	q := u
	if branch != "" {
		q += "?ref=" + url.QueryEscape(branch)
	}
	if err := g.do(ctx, "GET", q, nil, &existing); err == nil && existing.SHA != "" {
		in["sha"] = existing.SHA
	}
	return g.do(ctx, "PUT", u, in, nil)
}

func (g *GitHub) GetRepo(ctx context.Context, repo string) (Repo, error) {
	var r Repo
	err := g.do(ctx, "GET", "/repos/"+repo, nil, &r)
	return r, err
}

// ListRepos lists an organisation's repositories, falling back to a user's.
func (g *GitHub) ListRepos(ctx context.Context, org string) ([]Repo, error) {
	var all []Repo
	base := "/orgs/" + org + "/repos"
	for page := 1; page <= 10; page++ {
		var rs []Repo
		err := g.do(ctx, "GET", fmt.Sprintf("%s?per_page=100&page=%d", base, page), nil, &rs)
		if errors.Is(err, ErrNotFound) && page == 1 && strings.HasPrefix(base, "/orgs/") {
			base = "/users/" + org + "/repos"
			page = 0
			continue
		}
		if err != nil {
			return all, err
		}
		all = append(all, rs...)
		if len(rs) < 100 {
			break
		}
	}
	return all, nil
}

// CreateRepo makes a repository in org, from a template when one is given.
func (g *GitHub) CreateRepo(ctx context.Context, org, name, description, template string, private bool) (Repo, error) {
	var r Repo
	if template != "" {
		in := map[string]any{"owner": org, "name": name, "description": description, "private": private}
		err := g.do(ctx, "POST", "/repos/"+template+"/generate", in, &r)
		return r, err
	}
	in := map[string]any{"name": name, "description": description, "private": private, "auto_init": true}
	err := g.do(ctx, "POST", "/orgs/"+org+"/repos", in, &r)
	if errors.Is(err, ErrNotFound) {
		err = g.do(ctx, "POST", "/user/repos", in, &r)
	}
	return r, err
}

// ProtectBranch requires a review and the given checks on branch.
func (g *GitHub) ProtectBranch(ctx context.Context, repo, branch string, checks []string) error {
	var status any
	if len(checks) > 0 {
		status = map[string]any{"strict": true, "contexts": checks}
	}
	in := map[string]any{
		"required_status_checks":        status,
		"enforce_admins":                false,
		"required_pull_request_reviews": map[string]any{"required_approving_review_count": 1},
		"restrictions":                  nil,
	}
	return g.do(ctx, "PUT", fmt.Sprintf("/repos/%s/branches/%s/protection", repo, branch), in, nil)
}

func truncate(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}
