package github

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/MelloB1989/karmax/internal/safety"
	"github.com/MelloB1989/karmax/pkg/connectorkit"
)

// Acting on GitHub as the operator, from the account the work belongs to.
//
// The connector's own tools act as whichever single credential was stored for
// them. The operator works from several logins, and a review posted from the
// wrong one is a review by somebody else. These tools take the account from
// the call (an event names the account it came to), or else from the
// repository: its owner's own login, or the one login that can write to it.
// When two could, they ask rather than guess.
//
// Nothing here deletes. A repository or branch is removed only when the
// operator says so, and a merge never takes the head branch with it.

// Act is one thing done as the operator, for telling them afterwards.
type Act struct {
	Account string // the login it was done as
	Action  string // "commented on", "approved", "merged", …
	Repo    string
	Number  int
	URL     string
	Body    string // what was said, when something was
}

// Actor holds the operator's gh logins and acts as the right one.
type Actor struct {
	Logins Logins
	// OnAct is told after every write. Nil is fine.
	OnAct func(Act)

	mu     sync.Mutex
	listed time.Time
	known  []Account
	repos  map[string]repoAccess
}

const (
	// accountsTTL spares a `gh auth status` per call, which checks every
	// login over the network.
	accountsTTL = 2 * time.Minute
	reposTTL    = 10 * time.Minute
	diffLimit   = 60_000
)

// repoAccess is what one account may do in one repository.
type repoAccess struct {
	at      time.Time
	visible bool
	push    bool
	err     error
	// What the repository allows a merge to be; nil is GitHub's default (yes).
	AllowMerge  *bool `json:"allow_merge_commit"`
	AllowSquash *bool `json:"allow_squash_merge"`
	AllowRebase *bool `json:"allow_rebase_merge"`
}

const (
	repoParam    = `"repo":{"type":"string","description":"owner/name, or the GitHub link from an event (a pull request or issue link also gives the number)"}`
	numberParam  = `"number":{"type":"integer","description":"issue or pull request number"}`
	accountParam = `"account":{"type":"string","description":"the GitHub login to act as. For an event, pass the event's account. When omitted: the repository owner's own login, or the only one of the operator's logins that can write to it"}`
)

// Tools are the acting tools, named apart from the connector's own so both
// can be installed at once.
func (a *Actor) Tools() []connectorkit.Tool {
	return []connectorkit.Tool{
		{
			Name: "github.accounts",
			Description: "The GitHub accounts the operator is logged in to on this machine (through gh), and whether each works. " +
				"With a repo, also which account would act there.",
			Parameters: json.RawMessage(`{"type":"object","properties":{` + repoParam + `,` + accountParam + `}}`),
			Call:       a.accountsTool,
		},
		{
			Name: "github.read",
			Description: "Read an issue or pull request as the right account: state, labels, the latest comments, and for a pull request " +
				"its head, mergeability, reviews, failing checks and review-thread comments (with ids to reply to). diff=true adds the diff.",
			Parameters: json.RawMessage(`{"type":"object","properties":{` + repoParam + `,` + numberParam + `,` + accountParam + `,
				"comments":{"type":"integer","description":"how many of the latest comments to include (default 5, at most 20)"},
				"diff":{"type":"boolean","description":"include the pull request's diff (truncated)"}},"required":["repo"]}`),
			Call: a.read,
		},
		{
			Name: "github.reply",
			Description: "Comment on an issue or pull request as the operator, from the right account. With in_reply_to (a review " +
				"comment id from github.read), replies in that review thread instead.",
			Parameters: json.RawMessage(`{"type":"object","properties":{` + repoParam + `,` + numberParam + `,` + accountParam + `,
				"body":{"type":"string","description":"what to say, in Markdown"},
				"in_reply_to":{"type":"integer","description":"a review comment id, to answer in its thread"}},"required":["repo","body"]}`),
			Call: a.reply,
		},
		{
			Name:        "github.review",
			Description: "Submit a pull request review as the operator, from the right account: APPROVE, REQUEST_CHANGES or COMMENT.",
			Parameters: json.RawMessage(`{"type":"object","properties":{` + repoParam + `,` + numberParam + `,` + accountParam + `,
				"event":{"type":"string","enum":["APPROVE","REQUEST_CHANGES","COMMENT"]},
				"body":{"type":"string","description":"the review text; required to request changes or comment"},
				"sha":{"type":"string","description":"the head commit reviewed, so a push after reading is not approved unseen"}},"required":["repo","event"]}`),
			Call: a.review,
		},
		{
			Name: "github.merge",
			Description: "Merge a pull request as the operator, from the right account. Uses a method the repository allows unless one " +
				"is given; pass sha (the head you reviewed) so nothing pushed since is merged unseen. It never deletes the branch.",
			Parameters: json.RawMessage(`{"type":"object","properties":{` + repoParam + `,` + numberParam + `,` + accountParam + `,
				"method":{"type":"string","enum":["merge","squash","rebase"]},
				"sha":{"type":"string","description":"the head commit that must still be the head"},
				"commit_title":{"type":"string"},"commit_message":{"type":"string"}},"required":["repo"]}`),
			Call: a.merge,
		},
	}
}

// target is what a call is about.
type target struct {
	host, owner, name string
	number            int
}

func (t target) repo() string { return t.owner + "/" + t.name }

func (t target) ref() string {
	if t.number > 0 {
		return t.repo() + "#" + strconv.Itoa(t.number)
	}
	return t.repo()
}

// parseTarget reads repo (owner/name or a link) and number.
func parseTarget(in map[string]any, needNumber bool) (target, error) {
	raw, _ := in["repo"].(string)
	raw = strings.TrimSpace(raw)
	tg := target{host: defaultHost}
	if raw == "" {
		return tg, fmt.Errorf("github: need repo as owner/name or a GitHub link")
	}
	path := raw
	if strings.Contains(raw, "://") {
		u, err := url.Parse(raw)
		if err != nil || u.Host == "" {
			return tg, fmt.Errorf("github: %q is not a GitHub link", raw)
		}
		tg.host, path = u.Host, u.Path
	} else if first, rest, ok := strings.Cut(raw, "/"); ok && strings.Contains(first, ".") {
		tg.host, path = first, rest // github.com/owner/name
	}
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
		return tg, fmt.Errorf("github: need repo as owner/name, got %q", raw)
	}
	tg.owner, tg.name = parts[0], strings.TrimSuffix(parts[1], ".git")
	if len(parts) >= 4 && (parts[2] == "pull" || parts[2] == "pulls" || parts[2] == "issues") {
		tg.number, _ = strconv.Atoi(parts[3])
	}
	if n := intOf(in["number"], 0); n > 0 {
		tg.number = n
	}
	if needNumber && tg.number <= 0 {
		return tg, fmt.Errorf("github: need the issue or pull request number for %s", tg.repo())
	}
	return tg, nil
}

func sameHost(a, b string) bool {
	if a == "" {
		a = defaultHost
	}
	if b == "" {
		b = defaultHost
	}
	return strings.EqualFold(a, b)
}

// accounts lists the gh logins, briefly cached. An empty list is never
// cached: the operator may be logging in right now.
func (a *Actor) accounts(ctx context.Context) ([]Account, error) {
	a.mu.Lock()
	if len(a.known) > 0 && time.Since(a.listed) < accountsTTL {
		out := append([]Account(nil), a.known...)
		a.mu.Unlock()
		return out, nil
	}
	a.mu.Unlock()
	if a.Logins == nil {
		return nil, fmt.Errorf("github: no gh logins are wired in")
	}
	accts, err := a.Logins.Accounts(ctx)
	if err != nil {
		return nil, err
	}
	a.mu.Lock()
	if len(accts) > 0 {
		a.known, a.listed = append([]Account(nil), accts...), time.Now()
	}
	a.mu.Unlock()
	return accts, nil
}

// pick chooses the account to act as, and borrows its token.
func (a *Actor) pick(ctx context.Context, tg target, explicit string) (Account, string, error) {
	all, err := a.accounts(ctx)
	if err != nil {
		return Account{}, "", fmt.Errorf("github: listing the operator's GitHub logins: %w", err)
	}
	var onHost []Account
	for _, acc := range all {
		if sameHost(acc.Host, tg.host) {
			onHost = append(onHost, acc)
		}
	}

	if explicit = strings.TrimSpace(strings.TrimPrefix(explicit, "@")); explicit != "" {
		for _, acc := range onHost {
			if strings.EqualFold(acc.Name(), explicit) || strings.EqualFold(acc.Login, explicit) {
				if !acc.OK() {
					return Account{}, "", fmt.Errorf("github: gh reports %s as %s (%s) — `gh auth login` again for it",
						acc.Name(), acc.State, acc.Error)
				}
				tok, err := a.Logins.Token(ctx, acc)
				return acc, tok, err
			}
		}
		return Account{}, "", fmt.Errorf("github: %q is not a GitHub login on %s here; the operator is logged in as: %s",
			explicit, tg.host, names(onHost))
	}

	var usable []Account
	for _, acc := range onHost {
		if acc.OK() {
			usable = append(usable, acc)
		}
	}
	switch len(usable) {
	case 0:
		return Account{}, "", fmt.Errorf("github: no working GitHub login on %s — run `gh auth login`", tg.host)
	case 1:
		tok, err := a.Logins.Token(ctx, usable[0])
		return usable[0], tok, err
	}
	for _, acc := range usable {
		if strings.EqualFold(acc.Login, tg.owner) {
			tok, err := a.Logins.Token(ctx, acc)
			return acc, tok, err
		}
	}

	// Ask each login what it may do here.
	tokens := map[string]string{}
	var writers, readers []Account
	var firstErr error
	for _, acc := range usable {
		tok, err := a.Logins.Token(ctx, acc)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		tokens[acc.Name()] = tok
		access := a.access(ctx, acc, tok, tg)
		switch {
		case access.push:
			writers = append(writers, acc)
		case access.visible:
			readers = append(readers, acc)
		case access.err != nil && firstErr == nil:
			firstErr = access.err
		}
	}
	for _, group := range [][]Account{writers, readers} {
		if len(group) == 1 {
			return group[0], tokens[group[0].Name()], nil
		}
		if len(group) > 1 {
			return Account{}, "", fmt.Errorf("github: several of the operator's GitHub logins can act on %s (%s); "+
				"pass account= — for an event, the account it came to", tg.repo(), names(group))
		}
	}
	if firstErr != nil {
		return Account{}, "", fmt.Errorf("github: none of the operator's GitHub logins (%s) can see %s: %w",
			names(usable), tg.repo(), firstErr)
	}
	return Account{}, "", fmt.Errorf("github: none of the operator's GitHub logins (%s) can see %s", names(usable), tg.repo())
}

// access asks GitHub what one account may do in one repository.
func (a *Actor) access(ctx context.Context, acc Account, tok string, tg target) repoAccess {
	key := acc.Name() + " " + strings.ToLower(tg.repo())
	a.mu.Lock()
	if r, ok := a.repos[key]; ok && time.Since(r.at) < reposTTL {
		a.mu.Unlock()
		return r
	}
	a.mu.Unlock()

	var out struct {
		repoAccess
		Permissions struct {
			Admin    bool `json:"admin"`
			Maintain bool `json:"maintain"`
			Push     bool `json:"push"`
		} `json:"permissions"`
	}
	_, err := callAt(ctx, acc.APIBase(), tok, "GET", "/repos/"+tg.repo(), "", nil, &out)
	r := out.repoAccess
	r.at, r.err = time.Now(), err
	if err == nil {
		r.visible = true
		r.push = out.Permissions.Push || out.Permissions.Maintain || out.Permissions.Admin
	}
	a.mu.Lock()
	if a.repos == nil {
		a.repos = map[string]repoAccess{}
	}
	a.repos[key] = r
	a.mu.Unlock()
	return r
}

func names(accts []Account) string {
	var out []string
	for _, a := range accts {
		out = append(out, a.Name())
	}
	if len(out) == 0 {
		return "none"
	}
	return strings.Join(out, ", ")
}

func (a *Actor) told(act Act) {
	if a.OnAct != nil {
		a.OnAct(act)
	}
}

func str(in map[string]any, k string) string {
	s, _ := in[k].(string)
	return strings.TrimSpace(s)
}

func (a *Actor) accountsTool(ctx context.Context, _ connectorkit.Credentials, in map[string]any) (any, error) {
	all, err := a.accounts(ctx)
	if err != nil {
		return nil, fmt.Errorf("github: listing the operator's GitHub logins: %w", err)
	}
	rows := make([]map[string]any, 0, len(all))
	for _, acc := range all {
		row := map[string]any{"account": acc.Name(), "login": acc.Login, "host": acc.Host,
			"active_in_gh": acc.Active, "working": acc.OK()}
		if acc.Error != "" {
			row["error"] = acc.Error
		}
		rows = append(rows, row)
	}
	out := map[string]any{"accounts": rows}
	if str(in, "repo") != "" {
		tg, err := parseTarget(in, false)
		if err != nil {
			return nil, err
		}
		if acc, _, err := a.pick(ctx, tg, str(in, "account")); err != nil {
			out["choose"] = err.Error()
		} else {
			out["acts_as"] = acc.Name()
		}
	}
	return out, nil
}

func (a *Actor) reply(ctx context.Context, _ connectorkit.Credentials, in map[string]any) (any, error) {
	tg, err := parseTarget(in, true)
	if err != nil {
		return nil, err
	}
	body := str(in, "body")
	if body == "" {
		return nil, fmt.Errorf("github: a reply needs a body")
	}
	acc, tok, err := a.pick(ctx, tg, str(in, "account"))
	if err != nil {
		return nil, err
	}
	path, action := fmt.Sprintf("/repos/%s/issues/%d/comments", tg.repo(), tg.number), "commented on"
	if id := intOf(in["in_reply_to"], 0); id > 0 {
		path = fmt.Sprintf("/repos/%s/pulls/%d/comments/%d/replies", tg.repo(), tg.number, id)
		action = "replied in a review thread on"
	}
	payload, _ := json.Marshal(map[string]string{"body": body})
	var out struct {
		HTMLURL string `json:"html_url"`
	}
	if _, err := callAt(ctx, acc.APIBase(), tok, "POST", path, "", payload, &out); err != nil {
		return nil, err
	}
	a.told(Act{Account: acc.Name(), Action: action, Repo: tg.repo(), Number: tg.number, URL: out.HTMLURL, Body: body})
	return map[string]any{"as": acc.Name(), "posted": true, "url": out.HTMLURL}, nil
}

func (a *Actor) review(ctx context.Context, _ connectorkit.Credentials, in map[string]any) (any, error) {
	tg, err := parseTarget(in, true)
	if err != nil {
		return nil, err
	}
	event := strings.ToUpper(str(in, "event"))
	action := map[string]string{"APPROVE": "approved", "REQUEST_CHANGES": "requested changes on", "COMMENT": "reviewed"}[event]
	if action == "" {
		return nil, fmt.Errorf("github: event must be APPROVE, REQUEST_CHANGES or COMMENT, not %q", str(in, "event"))
	}
	body := str(in, "body")
	if body == "" && event != "APPROVE" {
		return nil, fmt.Errorf("github: a review that requests changes or comments needs a body saying what")
	}
	acc, tok, err := a.pick(ctx, tg, str(in, "account"))
	if err != nil {
		return nil, err
	}
	req := map[string]string{"event": event}
	if body != "" {
		req["body"] = body
	}
	if sha := str(in, "sha"); sha != "" {
		req["commit_id"] = sha
	}
	payload, _ := json.Marshal(req)
	var out struct {
		HTMLURL string `json:"html_url"`
		State   string `json:"state"`
	}
	if _, err := callAt(ctx, acc.APIBase(), tok, "POST", fmt.Sprintf("/repos/%s/pulls/%d/reviews", tg.repo(), tg.number),
		"", payload, &out); err != nil {
		return nil, err
	}
	a.told(Act{Account: acc.Name(), Action: action, Repo: tg.repo(), Number: tg.number, URL: out.HTMLURL, Body: body})
	return map[string]any{"as": acc.Name(), "state": out.State, "url": out.HTMLURL}, nil
}

func (a *Actor) merge(ctx context.Context, _ connectorkit.Credentials, in map[string]any) (any, error) {
	tg, err := parseTarget(in, true)
	if err != nil {
		return nil, err
	}
	method := strings.ToLower(str(in, "method"))
	if method != "" && method != "merge" && method != "squash" && method != "rebase" {
		return nil, fmt.Errorf("github: method must be merge, squash or rebase, not %q", method)
	}
	acc, tok, err := a.pick(ctx, tg, str(in, "account"))
	if err != nil {
		return nil, err
	}
	if method == "" {
		method = allowedMethod(a.access(ctx, acc, tok, tg))
	}
	req := map[string]string{"merge_method": method}
	for _, k := range []string{"sha", "commit_title", "commit_message"} {
		if v := str(in, k); v != "" {
			req[k] = v
		}
	}
	payload, _ := json.Marshal(req)
	var out struct {
		Merged  bool   `json:"merged"`
		SHA     string `json:"sha"`
		Message string `json:"message"`
	}
	// PUT …/merge only merges. The head branch stays unless the repository
	// itself is set to remove merged branches.
	if _, err := callAt(ctx, acc.APIBase(), tok, "PUT", fmt.Sprintf("/repos/%s/pulls/%d/merge", tg.repo(), tg.number),
		"", payload, &out); err != nil {
		return nil, err
	}
	link := fmt.Sprintf("https://%s/%s/pull/%d", tg.host, tg.repo(), tg.number)
	a.told(Act{Account: acc.Name(), Action: "merged", Repo: tg.repo(), Number: tg.number, URL: link})
	return map[string]any{"as": acc.Name(), "merged": out.Merged, "sha": out.SHA, "method": method,
		"message": out.Message, "url": link}, nil
}

// allowedMethod is GitHub's own default among what the repository allows.
func allowedMethod(r repoAccess) string {
	allowed := func(p *bool) bool { return p == nil || *p }
	switch {
	case allowed(r.AllowMerge):
		return "merge"
	case allowed(r.AllowSquash):
		return "squash"
	case allowed(r.AllowRebase):
		return "rebase"
	}
	return "merge"
}

func (a *Actor) read(ctx context.Context, _ connectorkit.Credentials, in map[string]any) (any, error) {
	tg, err := parseTarget(in, true)
	if err != nil {
		return nil, err
	}
	acc, tok, err := a.pick(ctx, tg, str(in, "account"))
	if err != nil {
		return nil, err
	}
	get := func(path, accept string, out any) error {
		_, err := callAt(ctx, acc.APIBase(), tok, "GET", path, accept, nil, out)
		return err
	}
	type login struct {
		Login string `json:"login"`
	}
	var issue struct {
		Title       string    `json:"title"`
		Body        string    `json:"body"`
		State       string    `json:"state"`
		StateReason string    `json:"state_reason"`
		HTMLURL     string    `json:"html_url"`
		Comments    int       `json:"comments"`
		User        login     `json:"user"`
		Assignees   []login   `json:"assignees"`
		PullRequest *struct{} `json:"pull_request"`
		Labels      []struct {
			Name string `json:"name"`
		} `json:"labels"`
	}
	base := fmt.Sprintf("/repos/%s", tg.repo())
	if err := get(fmt.Sprintf("%s/issues/%d", base, tg.number), "", &issue); err != nil {
		return nil, err
	}
	src := fmt.Sprintf("%s on GitHub, opened by %s", tg.ref(), issue.User.Login)
	out := map[string]any{
		"as": acc.Name(), "repo": tg.repo(), "number": tg.number, "url": issue.HTMLURL,
		"state": issue.State, "author": issue.User.Login, "is_pull_request": issue.PullRequest != nil,
		"title": safety.Fence(src, issue.Title), "body": safety.Fence(src, issue.Body), "comment_count": issue.Comments,
	}
	if issue.StateReason != "" {
		out["state_reason"] = issue.StateReason
	}
	var labels, assignees []string
	for _, l := range issue.Labels {
		labels = append(labels, l.Name)
	}
	for _, l := range issue.Assignees {
		assignees = append(assignees, l.Login)
	}
	out["labels"], out["assignees"] = labels, assignees
	var problems []string
	note := func(what string, err error) {
		if err != nil {
			problems = append(problems, what+": "+err.Error())
		}
	}

	limit := intOf(in["comments"], 5)
	if limit > 20 {
		limit = 20
	}
	type comment struct {
		ID        int64  `json:"id"`
		Body      string `json:"body"`
		HTMLURL   string `json:"html_url"`
		CreatedAt string `json:"created_at"`
		User      login  `json:"user"`
		Path      string `json:"path"`
		Line      int    `json:"line"`
		InReplyTo int64  `json:"in_reply_to_id"`
	}
	fenced := func(c comment) map[string]any {
		m := map[string]any{"id": c.ID, "author": c.User.Login, "at": c.CreatedAt, "url": c.HTMLURL,
			"body": safety.Fence(fmt.Sprintf("a comment by %s on %s", c.User.Login, tg.ref()), c.Body)}
		if c.Path != "" {
			m["path"], m["line"] = c.Path, c.Line
		}
		if c.InReplyTo != 0 {
			m["in_reply_to"] = c.InReplyTo
		}
		return m
	}
	if limit > 0 && issue.Comments > 0 {
		// Comments come oldest first; the last page holds the latest.
		var cs []comment
		last := (issue.Comments + 99) / 100
		err := get(fmt.Sprintf("%s/issues/%d/comments?per_page=100&page=%d", base, tg.number, last), "", &cs)
		note("comments", err)
		if len(cs) > limit {
			cs = cs[len(cs)-limit:]
		}
		var list []map[string]any
		for _, c := range cs {
			list = append(list, fenced(c))
		}
		out["comments"] = list
	}

	if issue.PullRequest != nil {
		var pr struct {
			Draft          bool   `json:"draft"`
			Merged         bool   `json:"merged"`
			Mergeable      *bool  `json:"mergeable"`
			MergeableState string `json:"mergeable_state"`
			Additions      int    `json:"additions"`
			Deletions      int    `json:"deletions"`
			ChangedFiles   int    `json:"changed_files"`
			Head           struct {
				Ref string `json:"ref"`
				SHA string `json:"sha"`
			} `json:"head"`
			Base struct {
				Ref string `json:"ref"`
			} `json:"base"`
			RequestedReviewers []login `json:"requested_reviewers"`
		}
		if err := get(fmt.Sprintf("%s/pulls/%d", base, tg.number), "", &pr); err != nil {
			note("pull request", err)
		} else {
			var requested []string
			for _, r := range pr.RequestedReviewers {
				requested = append(requested, r.Login)
			}
			out["draft"], out["merged"], out["mergeable_state"] = pr.Draft, pr.Merged, pr.MergeableState
			if pr.Mergeable != nil {
				out["mergeable"] = *pr.Mergeable
			}
			out["head"], out["head_sha"], out["base"] = pr.Head.Ref, pr.Head.SHA, pr.Base.Ref
			out["additions"], out["deletions"], out["changed_files"] = pr.Additions, pr.Deletions, pr.ChangedFiles
			out["requested_reviewers"] = requested
		}

		var reviews []struct {
			User  login  `json:"user"`
			State string `json:"state"`
		}
		if err := get(fmt.Sprintf("%s/pulls/%d/reviews?per_page=100", base, tg.number), "", &reviews); err != nil {
			note("reviews", err)
		} else {
			// Each reviewer's standing verdict: a later comment does not undo
			// an approval or a change request.
			latest := map[string]string{}
			for _, r := range reviews {
				if r.State == "COMMENTED" && latest[r.User.Login] != "" {
					continue
				}
				latest[r.User.Login] = r.State
			}
			out["reviews"] = latest
		}

		if pr.Head.SHA != "" {
			var checks struct {
				Runs []struct {
					Name       string `json:"name"`
					Status     string `json:"status"`
					Conclusion string `json:"conclusion"`
				} `json:"check_runs"`
			}
			if err := get(fmt.Sprintf("%s/commits/%s/check-runs?per_page=100", base, pr.Head.SHA), "", &checks); err != nil {
				note("checks", err)
			} else {
				var failing, pending []string
				passed := 0
				for _, r := range checks.Runs {
					switch {
					case r.Status != "completed":
						pending = append(pending, r.Name)
					case r.Conclusion == "failure" || r.Conclusion == "timed_out" || r.Conclusion == "action_required" ||
						r.Conclusion == "startup_failure":
						failing = append(failing, r.Name)
					default:
						passed++
					}
				}
				sort.Strings(failing)
				sort.Strings(pending)
				out["failing_checks"], out["pending_checks"], out["passed_checks"] = failing, pending, passed
			}
		}

		if limit > 0 {
			var rc []comment
			err := get(fmt.Sprintf("%s/pulls/%d/comments?per_page=%d&sort=created&direction=desc", base, tg.number, limit), "", &rc)
			note("review comments", err)
			if len(rc) > limit {
				rc = rc[:limit]
			}
			var list []map[string]any
			for i := len(rc) - 1; i >= 0; i-- { // oldest first, like the rest
				list = append(list, fenced(rc[i]))
			}
			out["review_comments"] = list
		}

		if b, _ := in["diff"].(bool); b {
			var diff string
			if err := get(fmt.Sprintf("%s/pulls/%d", base, tg.number), "application/vnd.github.diff", &diff); err != nil {
				note("diff", err)
			} else {
				if len(diff) > diffLimit {
					diff = diff[:diffLimit] + "\n… (diff truncated)"
				}
				out["diff"] = safety.Fence("the diff of "+tg.ref(), diff)
			}
		}
	}
	if len(problems) > 0 {
		out["incomplete"] = problems
	}
	return out, nil
}
