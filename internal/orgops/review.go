package orgops

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/MelloB1989/karmax/pkg/loopkit"
)

// ModelReview is what the model is asked to return.
type ModelReview struct {
	Verdict  string  `json:"verdict"` // approve | comment | request_changes
	Summary  string  `json:"summary"`
	Risk     float64 `json:"risk"`
	Comments []struct {
		Path     string `json:"path"`
		Line     int    `json:"line"`
		Severity string `json:"severity"`
		Body     string `json:"body"`
	} `json:"comments"`
}

// ReviewResult is what a review decided and did.
type ReviewResult struct {
	Repo     string    `json:"repo"`
	Number   int       `json:"number"`
	Title    string    `json:"title"`
	URL      string    `json:"url"`
	Event    string    `json:"event"`
	Verdict  string    `json:"verdict"`
	Risk     float64   `json:"risk"`
	Findings []Finding `json:"findings"`
	Summary  string    `json:"summary"`
	Merge    Mode      `json:"merge"`
	Posted   bool      `json:"posted"`
}

const maxDiffForModel = 60_000

// ReviewPR is the PR guardian: deterministic standards and threat checks, then
// a model review held to the org's written rules, then whatever the policy
// allows — post the review, approve, queue the merge or ask a human.
func (s *Service) ReviewPR(ctx context.Context, k loopkit.Kit, repo string, n int) (ReviewResult, error) {
	repo = s.cfg.FullName(repo)
	pol := s.cfg.PolicyFor(repo)
	res := ReviewResult{Repo: repo, Number: n}
	if pol.ModeFor(ActReview) == ModeOff {
		return res, nil
	}
	pr, err := s.gh.GetPR(ctx, repo, n)
	if err != nil {
		return res, fmt.Errorf("read PR %s#%d: %w", repo, n, err)
	}
	res.Title, res.URL = pr.Title, pr.HTMLURL
	if pr.State != "" && pr.State != "open" {
		return res, nil
	}
	// One review per commit: a webhook redelivery, or the incident flow and
	// the PR loop both reaching the same fix PR, must not review it twice.
	seenKey := fmt.Sprintf("%s#%d@%s", repo, n, pr.Head.SHA)
	if _, seen, _ := s.kv.KVGet(groupReviews+":seen", seenKey); seen && pr.Head.SHA != "" {
		var prev ReviewResult
		_, _ = s.getJSON(groupReviews, fmt.Sprintf("%s#%d", repo, n), &prev)
		return prev, nil
	}
	_ = s.kv.KVSet(groupReviews+":seen", seenKey, "1", 30*24*time.Hour)
	files, err := s.gh.ListPRFiles(ctx, repo, n)
	if err != nil {
		return res, fmt.Errorf("read PR files: %w", err)
	}

	findings := CheckPR(s.cfg.Standards, pr, files)
	findings = append(findings, MissingFiles(s.cfg.Standards, func(p string) bool {
		_, err := s.gh.GetFile(ctx, repo, p, pr.Base.Ref)
		return err == nil
	})...)
	threats := ScanDiff(s.cfg.Threats, files)
	findings = append(findings, threats...)

	mr, modelErr := s.modelReview(ctx, k, repo, pr, files, findings)
	if modelErr != nil {
		k.Logf("orgops: model review of %s#%d failed, posting checks only: %v", repo, n, modelErr)
	}
	var inline []ReviewComment
	for _, c := range mr.Comments {
		sev := Severity(strings.ToLower(c.Severity))
		if sev.rank() == 0 {
			sev = SevInfo
		}
		findings = append(findings, Finding{Rule: "review", Severity: sev, Path: c.Path, Line: c.Line, Message: c.Body})
		if c.Path != "" && c.Line > 0 {
			inline = append(inline, ReviewComment{Path: c.Path, Line: c.Line, Side: "RIGHT", Body: c.Body})
		}
	}
	for _, f := range threats {
		if f.Line > 0 {
			inline = append(inline, ReviewComment{Path: f.Path, Line: f.Line, Side: "RIGHT",
				Body: "🔒 **" + f.Rule + "** — " + string(f.Severity) + ". Remove it and rotate the credential if it was real."})
		}
	}
	sortFindings(findings)

	res.Findings = findings
	res.Summary = mr.Summary
	res.Risk = Risk(pr, files, findings)
	if mr.Risk > res.Risk {
		res.Risk = mr.Risk
	}
	blocked := worst(findings) == SevBlocker || mr.Verdict == "request_changes"
	res.Verdict = mr.Verdict
	if res.Verdict == "" {
		res.Verdict = "comment"
	}
	if blocked {
		res.Verdict = "request_changes"
	}

	self := s.cfg.GitHub.BotLogin != "" && strings.EqualFold(pr.User.Login, s.cfg.GitHub.BotLogin)
	switch {
	case blocked:
		res.Event = "REQUEST_CHANGES"
	case res.Verdict == "approve" && pol.Decide(ActApprovePR, res.Risk) == ModeAuto:
		res.Event = "APPROVE"
	default:
		res.Event = "COMMENT"
	}
	// GitHub refuses a review verdict on your own PR; a comment still lands.
	if self {
		res.Event = "COMMENT"
	}

	body := renderReview(res, pr)
	c, caseErr := k.CaseOpen(s.cfg.Agent, fmt.Sprintf("pr:%s#%d", repo, n), fmt.Sprintf("%s#%d %s", repo, n, pr.Title))
	caseID := ""
	if caseErr == nil {
		caseID = c.ID
		_ = k.CaseSetState(caseID, "review")
		_ = k.CaseLog(caseID, "review", fmt.Sprintf("%s risk=%.2f findings=%d", res.Event, res.Risk, len(findings)))
	}
	channel := s.cfg.ChannelFor("review", repo)

	switch pol.Decide(ActReview, 0) {
	case ModeAuto:
		if err := s.gh.CreateReview(ctx, repo, n, res.Event, body, inline); err != nil {
			return res, fmt.Errorf("post review: %w", err)
		}
		res.Posted = true
		_ = k.Audit("github.review", fmt.Sprintf("%s#%d", repo, n), res.Event, truncate(res.Summary, 200))
	case ModeApprove:
		_, _ = s.ask(ctx, k, Pending{Action: ActReview, Title: fmt.Sprintf("post review on %s#%d", repo, n),
			Repo: repo, Number: n, CaseID: caseID, Channel: channel,
			Args: map[string]any{"event": res.Event, "body": body}}, "The review is drafted.")
	}
	_ = s.putJSON(groupReviews, fmt.Sprintf("%s#%d", repo, n), res, 0)

	s.say(ctx, k, caseID, channel, renderChat(res, s.cfg.OwnersFor(repo), pr.User.Login))
	if len(threats) > 0 {
		if sec := s.cfg.ChannelFor("security", repo); sec != channel {
			s.say(ctx, k, "", sec, fmt.Sprintf("🔒 *Security*: %d finding(s) in %s#%d — %s", len(threats), repo, n, pr.HTMLURL))
		}
		_ = k.Notify("Security finding in "+repo, fmt.Sprintf("%d issue(s) in PR #%d", len(threats), n))
	}

	// A fix branch's merge belongs to its incident, which has its own policy.
	if !blocked && res.Verdict == "approve" && !pr.Draft && !strings.HasPrefix(pr.Head.Ref, FixBranchPrefix) {
		res.Merge = pol.Decide(ActMerge, res.Risk)
		s.queueMerge(ctx, k, Automerge{Repo: repo, Number: n, Action: ActMerge, CaseID: caseID, Channel: channel}, res.Merge, pr.Head.SHA,
			fmt.Sprintf("Review passed with risk %.2f.", res.Risk))
	}
	_ = k.Remember(fmt.Sprintf("[orgops] PR %s#%d %q by %s reviewed: %s (risk %.2f). %s",
		repo, n, pr.Title, pr.User.Login, res.Verdict, res.Risk, truncate(res.Summary, 300)))
	return res, nil
}

func (s *Service) modelReview(ctx context.Context, k loopkit.Kit, repo string, pr PR, files []PRFile, findings []Finding) (ModelReview, error) {
	var diff strings.Builder
	for _, f := range files {
		fmt.Fprintf(&diff, "--- %s (%s, +%d -%d)\n%s\n", f.Filename, f.Status, f.Additions, f.Deletions, f.Patch)
		if diff.Len() > maxDiffForModel {
			diff.WriteString("\n[diff truncated]\n")
			break
		}
	}
	var pre strings.Builder
	for _, f := range findings {
		fmt.Fprintf(&pre, "- [%s] %s: %s\n", f.Severity, f.Rule, f.Message)
	}
	if pre.Len() == 0 {
		pre.WriteString("(none)\n")
	}
	prompt := fmt.Sprintf(`You are the staff engineer reviewing pull requests for the %s organisation.
Review this PR strictly against the rules below, then for correctness, security and maintainability.
Do not modify any files and do not run git or gh — reading is enough.

%s

# Pull request
Repository: %s
PR #%d: %s
Author: %s   Branch: %s -> %s
Description:
%s

# Automated checks already run
%s
# Diff
%s

Reply with ONLY a JSON object, no prose around it:
{"verdict":"approve|comment|request_changes","summary":"2-4 sentences a busy lead can act on","risk":0.0-1.0,
 "comments":[{"path":"file","line":<line in new file>,"severity":"info|warning|blocker","body":"what and why, with a fix"}]}
Use request_changes only for real defects, security problems or clear rule violations. At most 8 comments.`,
		s.cfg.Org, s.rulesFor(ctx, repo, pr.Base.Ref), repo, pr.Number, pr.Title, pr.User.Login,
		pr.Head.Ref, pr.Base.Ref, fence(pr.Body), pre.String(), fence(diff.String()))
	out, err := k.Harness(ctx, prompt)
	if err != nil {
		return ModelReview{}, err
	}
	var mr ModelReview
	if err := extractJSON(out, &mr); err != nil {
		return ModelReview{Summary: truncate(strings.TrimSpace(out), 600)}, err
	}
	mr.Verdict = strings.ToLower(strings.TrimSpace(mr.Verdict))
	return mr, nil
}

// queueMerge applies the merge decision: merge on green CI, ask, or just say.
func (s *Service) queueMerge(ctx context.Context, k loopkit.Kit, am Automerge, mode Mode, sha, why string) {
	am.CreatedAt = s.now()
	label := fmt.Sprintf("%s#%d", am.Repo, am.Number)
	switch mode {
	case ModeAuto:
		_ = s.putJSON(groupAutomerge, label, am, 7*24*time.Hour)
		s.say(ctx, k, am.CaseID, am.Channel, fmt.Sprintf("🤖 %s will merge automatically once CI is green.", label))
		s.TryMerge(ctx, k, am, sha)
	case ModeApprove:
		_, _ = s.ask(ctx, k, Pending{Action: am.Action, Title: "merge " + label, Repo: am.Repo, Number: am.Number,
			CaseID: am.CaseID, Channel: am.Channel, Args: map[string]any{"incident": am.Incident}}, why)
	case ModeNotify:
		s.say(ctx, k, am.CaseID, am.Channel, fmt.Sprintf("ℹ️ %s is ready to merge (policy: notify only).", label))
	}
}

// TryMerge merges a queued PR if its CI is green, reports a red CI, and leaves
// a pending one queued. It is safe to call repeatedly.
func (s *Service) TryMerge(ctx context.Context, k loopkit.Kit, am Automerge, sha string) {
	label := fmt.Sprintf("%s#%d", am.Repo, am.Number)
	pr, err := s.gh.GetPR(ctx, am.Repo, am.Number)
	if err != nil {
		k.Logf("orgops: automerge %s: %v", label, err)
		return
	}
	if pr.Merged || pr.State == "closed" {
		_ = s.kv.KVDelete(groupAutomerge, label)
		return
	}
	if sha == "" {
		sha = pr.Head.SHA
	}
	state, err := s.gh.CIState(ctx, am.Repo, sha)
	if err != nil {
		k.Logf("orgops: CI state for %s: %v", label, err)
		return
	}
	switch state {
	case "pending":
		return
	case "failure":
		_ = s.kv.KVDelete(groupAutomerge, label)
		s.say(ctx, k, am.CaseID, am.Channel, fmt.Sprintf("🔴 CI failed on %s — not merging. %s", label, pr.HTMLURL))
		if am.Incident != "" {
			s.updateIncident(am.Incident, func(in *Incident) { in.State = "fix-ci-failed" })
		}
		return
	}
	// "none" is a repo with no CI, or CI that has not reported yet — the two
	// look the same, so only an org that said so merges without checks.
	if state == "none" && !s.cfg.MergeWithoutCI {
		return
	}
	if err := s.gh.Merge(ctx, am.Repo, am.Number, "squash", ""); err != nil {
		var apiErr *APIError
		if errors.As(err, &apiErr) && apiErr.Status == 405 {
			s.say(ctx, k, am.CaseID, am.Channel, fmt.Sprintf("⚠️ GitHub would not merge %s yet: %s", label, truncate(apiErr.Body, 200)))
			return
		}
		k.Logf("orgops: merge %s: %v", label, err)
		return
	}
	_ = s.kv.KVDelete(groupAutomerge, label)
	_ = k.Audit("github.merge", label, "merged", string(am.Action))
	s.say(ctx, k, am.CaseID, am.Channel, fmt.Sprintf("✅ Merged %s (CI %s). %s", label, state, pr.HTMLURL))
	if am.CaseID != "" {
		_ = k.CaseSetState(am.CaseID, "done")
	}
	if am.Incident != "" {
		s.updateIncident(am.Incident, func(in *Incident) { in.State = "fix-merged" })
	}
}

// SweepAutomerge re-checks every queued merge. It covers the CI webhook that
// never arrived, which is the common case for commit-status CI.
func (s *Service) SweepAutomerge(ctx context.Context, k loopkit.Kit) {
	ams, err := listJSON[Automerge](s, groupAutomerge)
	if err != nil {
		return
	}
	for _, am := range ams {
		s.TryMerge(ctx, k, am, "")
	}
}

// OnCheckSuite reacts to a finished CI run on a queued PR.
func (s *Service) OnCheckSuite(ctx context.Context, k loopkit.Kit, payload map[string]any) {
	repo, n := str(payload, "repo"), num(payload, "number")
	if n == 0 {
		if pr, ok, err := s.gh.FindPRByHead(ctx, repo, str(payload, "branch")); err == nil && ok {
			n = pr.Number
		}
	}
	if n == 0 {
		return
	}
	var am Automerge
	if ok, _ := s.getJSON(groupAutomerge, fmt.Sprintf("%s#%d", repo, n), &am); ok {
		s.TryMerge(ctx, k, am, str(payload, "sha"))
	}
}

func renderReview(r ReviewResult, pr PR) string {
	var b strings.Builder
	icon := map[string]string{"APPROVE": "✅", "REQUEST_CHANGES": "⛔", "COMMENT": "💬"}[r.Event]
	fmt.Fprintf(&b, "### %s KARMAX review — %s\n\n", icon, strings.ReplaceAll(r.Verdict, "_", " "))
	if r.Summary != "" {
		b.WriteString(r.Summary + "\n\n")
	}
	fmt.Fprintf(&b, "**Risk:** %.2f\n\n", r.Risk)
	if len(r.Findings) > 0 {
		b.WriteString("| Severity | Check | Finding |\n|---|---|---|\n")
		for _, f := range r.Findings {
			loc := ""
			if f.Path != "" {
				loc = "`" + f.Path
				if f.Line > 0 {
					loc += fmt.Sprintf(":%d", f.Line)
				}
				loc += "` "
			}
			fmt.Fprintf(&b, "| %s | %s | %s%s |\n", f.Severity, f.Rule, loc, strings.ReplaceAll(f.Message, "|", "\\|"))
		}
	} else {
		b.WriteString("No findings. Meets the org standards.\n")
	}
	b.WriteString("\n<sub>Reviewed against the organisation rules by KARMAX.</sub>")
	return b.String()
}

func renderChat(r ReviewResult, owners []string, author string) string {
	icon := map[string]string{"APPROVE": "✅", "REQUEST_CHANGES": "⛔", "COMMENT": "💬"}[r.Event]
	counts := map[Severity]int{}
	for _, f := range r.Findings {
		counts[f.Severity]++
	}
	msg := fmt.Sprintf("%s *PR review* %s#%d — %s\nby %s · verdict *%s* · risk %.2f · %d blocker / %d warning\n%s",
		icon, r.Repo, r.Number, r.Title, author, strings.ReplaceAll(r.Verdict, "_", " "), r.Risk,
		counts[SevBlocker], counts[SevWarning], r.URL)
	if r.Summary != "" {
		msg += "\n> " + truncate(r.Summary, 400)
	}
	if counts[SevBlocker] > 0 {
		for _, f := range r.Findings {
			if f.Severity == SevBlocker {
				msg += "\n• " + truncate(f.Message, 160)
			}
		}
	}
	if m := mention(owners); m != "" {
		msg += "\ncc " + m
	}
	return msg
}

var jsonObject = regexp.MustCompile(`(?s)\{.*\}`)

// extractJSON pulls the first JSON object out of a model reply that may have
// wrapped it in prose or a code fence.
func extractJSON(out string, v any) error {
	out = strings.TrimSpace(out)
	if err := json.Unmarshal([]byte(out), v); err == nil {
		return nil
	}
	m := jsonObject.FindString(out)
	if m == "" {
		return fmt.Errorf("no JSON object in reply")
	}
	return json.Unmarshal([]byte(m), v)
}

func fence(s string) string {
	return "```\n" + strings.ReplaceAll(s, "```", "ʼʼʼ") + "\n```"
}
