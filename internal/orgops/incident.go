package orgops

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/MelloB1989/karmax/pkg/loopkit"
)

// Bus event kinds the office publishes and consumes.
const (
	EventAlert   = "orgops.alert"   // an alert from a probe, a monitoring tool or a person
	EventFix     = "orgops.fix"     // send a coding agent to fix an incident
	EventReview  = "orgops.review"  // review a PR on request
	EventCommand = "orgops.command" // approve / reject / merge / status / create repo
	EventIndex   = "orgops.index"   // (re)index repositories into the knowledge store
)

// Incident is a production problem the office is handling.
type Incident struct {
	ID         string     `json:"id"`
	Key        string     `json:"key"`
	Source     string     `json:"source"`
	Service    string     `json:"service"`
	Repo       string     `json:"repo"`
	Title      string     `json:"title"`
	Detail     string     `json:"detail"`
	Severity   string     `json:"severity"`
	URL        string     `json:"url,omitempty"`
	CaseID     string     `json:"case_id"`
	Channel    string     `json:"channel"`
	State      string     `json:"state"`
	Evidence   string     `json:"evidence,omitempty"`
	PRNumber   int        `json:"pr_number,omitempty"`
	PRURL      string     `json:"pr_url,omitempty"`
	RootCause  string     `json:"root_cause,omitempty"`
	OpenedAt   time.Time  `json:"opened_at"`
	ResolvedAt *time.Time `json:"resolved_at,omitempty"`
}

func (s *Service) incidentByKey(key string) (Incident, bool) {
	all, _ := listJSON[Incident](s, groupIncidents)
	for _, in := range all {
		if in.Key == key && in.State != "resolved" {
			return in, true
		}
	}
	return Incident{}, false
}

// IncidentByID finds an incident by its INC- id (case-insensitive).
func (s *Service) IncidentByID(id string) (Incident, bool) {
	var in Incident
	ok, _ := s.getJSON(groupIncidents, strings.ToUpper(strings.TrimSpace(id)), &in)
	return in, ok
}

func (s *Service) saveIncident(in Incident) {
	_ = s.putJSON(groupIncidents, in.ID, in, 30*24*time.Hour)
}

func (s *Service) updateIncident(id string, fn func(*Incident)) {
	in, ok := s.IncidentByID(id)
	if !ok {
		return
	}
	fn(&in)
	s.saveIncident(in)
}

func severityRisk(sev string) float64 {
	switch strings.ToLower(sev) {
	case "critical", "fatal", "page", "p1":
		return 0.45
	case "info", "low", "p4":
		return 0.1
	}
	return 0.3
}

// HandleAlert is the incident autopilot's front door: open (or recover) an
// incident, tell the owners where they will see it, then investigate and fix
// as far as the policy allows.
func (s *Service) HandleAlert(ctx context.Context, k loopkit.Kit, a Alert) (Incident, error) {
	if a.Key == "" {
		a.Key = "alert:" + a.Service + ":" + a.Title
	}
	if a.Repo == "" && a.Service != "" {
		a.Repo = s.cfg.RepoForService(a.Service)
	}
	if a.Repo != "" {
		a.Repo = s.cfg.FullName(a.Repo)
	}

	if existing, open := s.incidentByKey(a.Key); open {
		if a.Resolved {
			now := s.now()
			existing.State, existing.ResolvedAt = "resolved", &now
			s.saveIncident(existing)
			s.say(ctx, k, existing.CaseID, existing.Channel,
				fmt.Sprintf("✅ *Resolved* %s — %s. %s", existing.ID, existing.Title, a.Detail))
			if existing.CaseID != "" {
				_ = k.CaseSetState(existing.CaseID, "done")
			}
			_ = k.Remember(fmt.Sprintf("[orgops] incident %s on %s resolved: %s. Root cause: %s. Fix: %s",
				existing.ID, first(existing.Service, existing.Repo), existing.Title, first(existing.RootCause, "unknown"), first(existing.PRURL, "none")))
			return existing, nil
		}
		_ = k.CaseLog(existing.CaseID, "alert", truncate(a.Detail, 300))
		return existing, nil
	}
	if a.Resolved {
		return Incident{}, nil
	}

	in := Incident{
		ID: "INC-" + newCode(), Key: a.Key, Source: a.Source, Service: a.Service, Repo: a.Repo,
		Title: a.Title, Detail: a.Detail, Severity: first(a.Severity, "warning"), URL: a.URL,
		Evidence: a.Evidence, State: "open", OpenedAt: s.now(),
	}
	in.Channel = s.cfg.ChannelFor("incident", in.Repo)
	if c, err := k.CaseOpen(s.cfg.Agent, "incident:"+in.ID, in.Title); err == nil {
		in.CaseID = c.ID
		_ = k.CaseSetState(c.ID, "open")
	}
	owners := s.ownersForIncident(in)
	s.saveIncident(in)
	_ = k.Audit("incident.open", first(in.Repo, in.Service), in.Severity, in.Title)

	msg := fmt.Sprintf("🚨 *Incident %s* [%s] %s\n%s", in.ID, strings.ToUpper(in.Severity), in.Title, in.Detail)
	if in.Service != "" || in.Repo != "" {
		msg += fmt.Sprintf("\nservice: %s · repo: %s", first(in.Service, "?"), first(in.Repo, "unknown"))
	}
	if in.URL != "" {
		msg += "\n" + in.URL
	}
	if m := mention(owners); m != "" {
		msg += "\nOwners: " + m
	}
	s.say(ctx, k, in.CaseID, in.Channel, msg)
	_ = k.Notify("Incident "+in.ID, in.Title)

	if in.Repo == "" {
		s.say(ctx, k, in.CaseID, in.Channel, "I can't map this to a repository, so I won't attempt a fix. "+
			"Add `service:` to the repo's entry in org_ops.repos, or reply `fix "+in.ID+" in <org/repo>`.")
		return in, nil
	}
	switch s.cfg.PolicyFor(in.Repo).Decide(ActIncidentFix, severityRisk(in.Severity)) {
	case ModeAuto:
		s.say(ctx, k, in.CaseID, in.Channel, "🛠️ Sending an engineer agent to find the root cause and fix it.")
		_ = s.publish(EventFix, map[string]any{"incident": in.ID})
	case ModeApprove:
		_, _ = s.ask(ctx, k, Pending{Action: ActIncidentFix, Title: "fix " + in.ID + " in " + in.Repo,
			Repo: in.Repo, CaseID: in.CaseID, Channel: in.Channel, Args: map[string]any{"incident": in.ID}},
			"I can send an agent to root-cause this, fix it and open a PR.")
		s.updateIncident(in.ID, func(x *Incident) { x.State = "awaiting-approval" })
	case ModeNotify:
		s.say(ctx, k, in.CaseID, in.Channel, "Reply `fix "+in.ID+"` and I'll send an agent to fix it.")
	}
	return in, nil
}

func (s *Service) ownersForIncident(in Incident) []string {
	for _, p := range s.cfg.Probes {
		if in.Key == "probe:"+p.Name && len(p.Owners) > 0 {
			return p.Owners
		}
	}
	return s.cfg.OwnersFor(in.Repo)
}

// FixBranchPrefix marks branches the office's own fixes are pushed to.
const FixBranchPrefix = "karmax/fix-"

// FixResult is what a dispatched agent reports back.
type FixResult struct {
	PRURL     string `json:"pr_url"`
	RootCause string `json:"root_cause"`
	Summary   string `json:"summary"`
}

var resultLine = regexp.MustCompile(`(?m)^.*KARMAX_RESULT\s*(\{.*\})\s*$`)
var prURL = regexp.MustCompile(`https?://[^\s"')]+/pull/(\d+)`)

// FixIncident sends a coding agent at an incident and drives its PR to merge.
// repoOverride lets a person name the repository when the alert could not.
func (s *Service) FixIncident(ctx context.Context, k loopkit.Kit, id, repoOverride string) error {
	in, ok := s.IncidentByID(id)
	if !ok {
		return fmt.Errorf("no incident %s", id)
	}
	if repoOverride != "" {
		in.Repo = s.cfg.FullName(repoOverride)
	}
	if in.Repo == "" {
		return fmt.Errorf("incident %s has no repository to fix", in.ID)
	}
	if in.State == "fixing" || in.PRNumber != 0 {
		s.say(ctx, k, in.CaseID, in.Channel, fmt.Sprintf("%s already has a fix in progress (%s).", in.ID, first(in.PRURL, in.State)))
		return nil
	}
	in.State = "fixing"
	s.saveIncident(in)
	if in.CaseID != "" {
		_ = k.CaseSetState(in.CaseID, "building")
	}
	branch := FixBranchPrefix + strings.ToLower(in.ID)
	brief := s.fixBrief(ctx, in, branch)

	out, err := s.dispatch(ctx, k, in, branch, brief)
	if err != nil {
		s.updateIncident(in.ID, func(x *Incident) { x.State = "fix-failed" })
		s.say(ctx, k, in.CaseID, in.Channel, fmt.Sprintf("❌ The fix attempt for %s failed: %s\nReply `fix %s` to try again.",
			in.ID, truncate(err.Error(), 300), in.ID))
		// Reported, and a retry is a person's call: a loop retry would post
		// the same failure again and spend another agent run.
		k.Logf("orgops: fix %s: %v", in.ID, err)
		return nil
	}
	var fr FixResult
	if m := resultLine.FindStringSubmatch(out); m != nil {
		_ = json.Unmarshal([]byte(m[1]), &fr)
	}
	pr, found, _ := s.gh.FindPRByHead(ctx, in.Repo, branch)
	if !found {
		if m := prURL.FindStringSubmatch(first(fr.PRURL, out)); m != nil {
			var n int
			fmt.Sscan(m[1], &n)
			if got, err := s.gh.GetPR(ctx, in.Repo, n); err == nil {
				pr, found = got, true
			}
		}
	}
	if !found {
		s.updateIncident(in.ID, func(x *Incident) { x.State = "fix-failed"; x.RootCause = fr.RootCause })
		s.say(ctx, k, in.CaseID, in.Channel, fmt.Sprintf("⚠️ The agent finished but opened no PR for %s.\n%s",
			in.ID, truncate(first(fr.Summary, out), 600)))
		return nil
	}
	s.updateIncident(in.ID, func(x *Incident) {
		x.State, x.PRNumber, x.PRURL, x.RootCause = "fix-pr-open", pr.Number, pr.HTMLURL, fr.RootCause
	})
	msg := fmt.Sprintf("🔧 Fix for %s is up: %s#%d — %s\n%s", in.ID, in.Repo, pr.Number, pr.Title, pr.HTMLURL)
	if fr.RootCause != "" {
		msg += "\n*Root cause:* " + truncate(fr.RootCause, 400)
	}
	s.say(ctx, k, in.CaseID, in.Channel, msg)
	_ = k.CaseLog(in.CaseID, "pr", pr.HTMLURL)

	// The guardian reviews the fixer: the fix is held to the same rules as any PR.
	if _, err := s.ReviewPR(ctx, k, in.Repo, pr.Number); err != nil {
		k.Logf("orgops: review of fix %s#%d: %v", in.Repo, pr.Number, err)
	}
	mode := s.cfg.PolicyFor(in.Repo).Decide(ActIncidentMerge, severityRisk(in.Severity))
	s.queueMerge(ctx, k, Automerge{Repo: in.Repo, Number: pr.Number, Action: ActIncidentMerge,
		CaseID: in.CaseID, Channel: in.Channel, Incident: in.ID}, mode, pr.Head.SHA,
		fmt.Sprintf("This merges the fix for %s to production.", in.ID))
	return nil
}

func (s *Service) fixBrief(ctx context.Context, in Incident, branch string) string {
	return fmt.Sprintf(`Production incident %s in the %s organisation. You are the on-call engineer.

Incident: %s
Severity: %s
Service: %s
Detail: %s
%s
Repository: %s

Do this:
1. Clone %s (gh repo clone or git clone) into a fresh directory and create the branch %s from the default branch.
2. Find the root cause from the code and the evidence above. Be specific.
3. Make the smallest correct fix. Add or update a test that would have caught it. Run the tests.
4. Commit, push the branch and open a pull request against the default branch titled "fix: <what you fixed>".
   The PR body must say: Incident %s, the root cause, the fix, and how it was tested.
5. Do NOT merge the PR — the org's merge policy decides that.

Follow these rules exactly:
%s

When you are done, print one final line exactly like:
KARMAX_RESULT {"pr_url":"<url of the PR>","root_cause":"<one sentence>","summary":"<what you changed>"}`,
		in.ID, s.cfg.Org, in.Title, in.Severity, first(in.Service, "unknown"), in.Detail,
		evidenceBlock(in.Evidence), in.Repo, in.Repo, branch, in.ID, s.rulesFor(ctx, in.Repo, ""))
}

func evidenceBlock(e string) string {
	if strings.TrimSpace(e) == "" {
		return ""
	}
	return "Recent checks:\n" + e
}

// dispatch hands the brief to whoever does the coding in this install.
func (s *Service) dispatch(ctx context.Context, k loopkit.Kit, in Incident, branch, brief string) (string, error) {
	timeout := s.cfg.Fixer.timeout()
	switch s.cfg.Fixer.Dispatcher {
	case "fleet":
		ctx, cancel := context.WithTimeout(ctx, timeout+time.Minute)
		defer cancel()
		out, err := s.runCmd(ctx, brief, s.cfg.Fixer.FleetctlPath, "run", "-",
			"--repo", in.Repo, "--task", strings.ToLower(in.ID), "--from", s.cfg.Agent,
			"--wait", "--timeout", timeout.String())
		if err != nil {
			return out, fmt.Errorf("fleetctl run: %w: %s", err, truncate(out, 300))
		}
		return out, nil
	case "sandbox":
		res, err := k.Sandbox(ctx, "fix-"+in.ID, loopkit.SandboxSpec{
			CaseID: in.CaseID, Repo: in.Repo, Branch: branch, Task: brief,
			Image: s.cfg.Fixer.Image, Timeout: timeout,
		})
		if err != nil {
			return "", err
		}
		if res.ExitCode != 0 {
			return res.LogTail, fmt.Errorf("sandbox exited %d", res.ExitCode)
		}
		return res.LogTail, nil
	default:
		ctx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		res, err := k.HarnessWith(ctx, loopkit.HarnessSpec{Prompt: brief, WorkingDir: s.cfg.Fixer.WorkDir, Ephemeral: true})
		return res.Output, err
	}
}

// RecordProbe turns a probe transition into an alert on the bus, so the probe
// loop stays fast and the incident loop does the slow work.
func (s *Service) RecordProbe(t Transition) error {
	a := Alert{
		Key: "probe:" + t.Probe.Name, Source: "probe", Title: t.Title, Detail: t.Detail,
		Service: t.Probe.Name, Repo: t.Probe.Repo, Severity: "critical", Resolved: !t.Firing,
		Evidence: t.Samples,
	}
	return s.publish(EventAlert, a.Payload())
}
