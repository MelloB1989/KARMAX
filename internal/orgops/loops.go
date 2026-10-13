package orgops

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/MelloB1989/karmax/internal/tools"
	"github.com/MelloB1989/karmax/pkg/loopkit"
)

// Loops are the office's workers. They are split by how long their work takes:
// a 40-minute fix must not hold up a PR review or a person's "approve", and the
// probe loop must stay fast enough to keep its own cadence.
func (s *Service) Loops() []loopkit.Loop {
	loops := []loopkit.Loop{
		{
			Name:        "orgops-pr",
			Description: "PR guardian: reviews every pull request in the org against its written rules, standards and a security scan, and merges per policy once CI is green.",
			Events:      []string{"github.pr.opened", "github.pr.updated", "github.pr.reopened", "github.check_suite.completed", EventReview},
			Run: func(ctx context.Context, k loopkit.Kit) error {
				p := k.Trigger().Payload
				switch str(p, "event_kind") {
				case "github.check_suite.completed":
					s.OnCheckSuite(ctx, k, p)
					return nil
				}
				if draft, _ := p["draft"].(bool); draft {
					return nil
				}
				repo, n := str(p, "repo"), num(p, "number")
				if repo == "" || n == 0 {
					return nil
				}
				if !s.ownsRepo(repo) {
					return nil
				}
				res, err := s.ReviewPR(ctx, k, repo, n)
				if err != nil {
					return err
				}
				k.Logf("orgops: reviewed %s#%d: %s (risk %.2f)", repo, n, res.Event, res.Risk)
				return nil
			},
		},
		{
			Name:        "orgops-incident",
			Description: "Incident autopilot: opens an incident for each alert, tells the owners, sends a coding agent to fix it and drives the fix to merge per policy.",
			Events:      []string{EventAlert, EventFix},
			Run: func(ctx context.Context, k loopkit.Kit) error {
				p := k.Trigger().Payload
				if str(p, "event_kind") == EventFix {
					return s.FixIncident(ctx, k, str(p, "incident"), str(p, "repo"))
				}
				_, err := s.HandleAlert(ctx, k, AlertFromPayload(p))
				return err
			},
		},
		{
			Name:        "orgops-command",
			Description: "Carries out what people ask the org office for in any chat: approve, reject, fix, merge, review, create repo, index, status.",
			Events:      []string{EventCommand, "comms.message"},
			Run: func(ctx context.Context, k loopkit.Kit) error {
				p := k.Trigger().Payload
				if str(p, "event_kind") == "comms.message" {
					if str(p, "direction") == "outbound" || !s.listensOn(str(p, "karmax_channel_id"), str(p, "channel_type")) {
						return nil
					}
					cmd, ok := ParseCommand(str(p, "content"))
					if !ok {
						return nil
					}
					replyTo := str(p, "karmax_channel_id") + ":" + str(p, "channel_id")
					return s.HandleCommand(ctx, k, cmd, replyTo, str(p, "sender_name"))
				}
				return s.HandleCommand(ctx, k, CommandFromPayload(p), str(p, "reply_to"), str(p, "requester"))
			},
		},
		{
			Name:        "orgops-watch",
			Description: "Probes production endpoints and re-checks queued merges, so a failing service is caught without waiting for a page.",
			Schedule:    loopkit.Every(s.cfg.ProbeEvery),
			Run: func(ctx context.Context, k loopkit.Kit) error {
				for _, t := range s.RunProbes(ctx) {
					if err := s.RecordProbe(t); err != nil {
						k.Logf("orgops: queue probe alert: %v", err)
					}
				}
				s.SweepAutomerge(ctx, k)
				return nil
			},
		},
		{
			Name:        "orgops-knowledge",
			Description: "Keeps a knowledge card of every repository in long-term memory, from the repositories' own documentation.",
			Events:      []string{EventIndex, "github.pr.merged"},
			Schedule:    s.knowledgeSchedule(),
			Run: func(ctx context.Context, k loopkit.Kit) error {
				p := k.Trigger().Payload
				repo := str(p, "repo")
				if repo != "" && !s.ownsRepo(s.cfg.FullName(repo)) {
					return nil
				}
				if repo == "" || repo == "all" {
					n, err := s.IndexAll(ctx, k)
					if err == nil && str(p, "reply_to") != "" {
						s.say(ctx, k, "", str(p, "reply_to"), fmt.Sprintf("📚 Indexed %d repositories into the knowledge store.", n))
					}
					return err
				}
				card, err := s.IndexRepo(ctx, k, repo)
				if err == nil && str(p, "reply_to") != "" {
					s.say(ctx, k, "", str(p, "reply_to"), fmt.Sprintf("📚 Indexed %s:\n%s", card.Repo, truncate(card.Card, 800)))
				}
				return err
			},
		},
	}
	return loops
}

func (s *Service) knowledgeSchedule() loopkit.Schedule {
	if _, err := time.ParseDuration(s.cfg.Knowledge.Every); err == nil {
		return loopkit.Every(s.cfg.Knowledge.Every)
	}
	return loopkit.Schedule{}
}

// ownsRepo keeps the office to its own org, whatever else the GitHub App sees.
func (s *Service) ownsRepo(full string) bool {
	return strings.HasPrefix(strings.ToLower(full), strings.ToLower(s.cfg.Org)+"/")
}

// listensOn says whether chat commands on a channel are for the office.
// Personal channels (WhatsApp) are left out unless named, so a "status" in a
// family group never reaches it.
func (s *Service) listensOn(channelID, channelType string) bool {
	if len(s.cfg.ListenChannels) == 0 {
		return channelType != "whatsapp"
	}
	for _, c := range s.cfg.ListenChannels {
		if c == channelID || c == "*" {
			return true
		}
	}
	return false
}

// --- agent tools --------------------------------------------------------------

type funcTool struct {
	name, desc string
	params     string
	run        func(ctx context.Context, in map[string]any) (any, error)
}

func (t *funcTool) Manifest() tools.ToolManifest {
	return tools.ToolManifest{Name: t.name, Description: t.desc, Parameters: json.RawMessage(t.params)}
}

func (t *funcTool) Execute(ctx context.Context, in map[string]any) (tools.ToolResult, error) {
	out, err := t.run(ctx, in)
	if err != nil {
		return tools.ErrorResult(err), nil
	}
	return tools.SuccessResult(out), nil
}

// Tools are what the orchestrator agent uses when a person asks for org work
// in their own words ("payments is throwing 500s, fix it") on any channel.
// Long work is queued to the loops and reported in chat; the tool returns at once.
func (s *Service) Tools() []tools.Tool {
	queued := func(what string) string {
		return what + " — queued. Progress is posted to " + first(s.cfg.Channels.Default, "the org channel") + "."
	}
	return []tools.Tool{
		&funcTool{name: "orgops_status", desc: "Org engineering office status: open incidents, approvals waiting, queued merges, probe health.",
			params: `{"type":"object","properties":{}}`,
			run:    func(ctx context.Context, in map[string]any) (any, error) { return s.Status(), nil }},
		&funcTool{name: "orgops_report_incident",
			desc:   "Report a production problem to the org office (e.g. 'payments API returning 500s'). It opens an incident, alerts the owners and, per policy, sends an agent to fix it and open a PR.",
			params: `{"type":"object","properties":{"title":{"type":"string"},"detail":{"type":"string"},"service":{"type":"string"},"repo":{"type":"string","description":"org/repo if known"},"severity":{"type":"string","enum":["info","warning","critical"]}},"required":["title"]}`,
			run: func(ctx context.Context, in map[string]any) (any, error) {
				a := Alert{Source: "chat", Title: str(in, "title"), Detail: str(in, "detail"), Service: str(in, "service"),
					Repo: str(in, "repo"), Severity: first(str(in, "severity"), "critical")}
				a.Key = "chat:" + a.Service + ":" + a.Title
				return queued("incident reported"), s.publish(EventAlert, a.Payload())
			}},
		&funcTool{name: "orgops_fix_incident", desc: "Send a coding agent to fix an open incident (INC-XXXXX) and open a PR. Optionally name the repo.",
			params: `{"type":"object","properties":{"incident":{"type":"string"},"repo":{"type":"string"}},"required":["incident"]}`,
			run: func(ctx context.Context, in map[string]any) (any, error) {
				return queued("fix"), s.publish(EventFix, map[string]any{"incident": strings.ToUpper(str(in, "incident")), "repo": str(in, "repo")})
			}},
		&funcTool{name: "orgops_approve", desc: "Approve an action the org office is waiting on, by its 5-character code.",
			params: `{"type":"object","properties":{"code":{"type":"string"},"requester":{"type":"string"}},"required":["code"]}`,
			run: func(ctx context.Context, in map[string]any) (any, error) {
				c := Command{Verb: "approve", Code: strings.ToUpper(str(in, "code"))}
				return queued("approval"), s.publish(EventCommand, c.Payload("", str(in, "requester")))
			}},
		&funcTool{name: "orgops_reject", desc: "Reject an action the org office is waiting on, by its code.",
			params: `{"type":"object","properties":{"code":{"type":"string"},"requester":{"type":"string"}},"required":["code"]}`,
			run: func(ctx context.Context, in map[string]any) (any, error) {
				c := Command{Verb: "reject", Code: strings.ToUpper(str(in, "code"))}
				return queued("rejection"), s.publish(EventCommand, c.Payload("", str(in, "requester")))
			}},
		&funcTool{name: "orgops_review_pr", desc: "Have the org office review a pull request against the org's rules and standards.",
			params: `{"type":"object","properties":{"repo":{"type":"string"},"number":{"type":"integer"}},"required":["repo","number"]}`,
			run: func(ctx context.Context, in map[string]any) (any, error) {
				return queued("review"), s.publish(EventReview, map[string]any{"repo": s.cfg.FullName(str(in, "repo")), "number": num(in, "number")})
			}},
		&funcTool{name: "orgops_merge_pr", desc: "Merge a pull request once its CI is green (a person asking counts as the approval).",
			params: `{"type":"object","properties":{"repo":{"type":"string"},"number":{"type":"integer"}},"required":["repo","number"]}`,
			run: func(ctx context.Context, in map[string]any) (any, error) {
				c := Command{Verb: "merge", Repo: str(in, "repo"), Number: num(in, "number")}
				return queued("merge"), s.publish(EventCommand, c.Payload("", ""))
			}},
		&funcTool{name: "orgops_create_repo", desc: "Create a repository in the org from a template, with the org's standard files and branch protection (per policy, may need approval).",
			params: `{"type":"object","properties":{"name":{"type":"string"},"template":{"type":"string"},"description":{"type":"string"},"requester":{"type":"string"}},"required":["name"]}`,
			run: func(ctx context.Context, in map[string]any) (any, error) {
				c := Command{Verb: "create", Name: str(in, "name"), Template: str(in, "template"), Description: str(in, "description")}
				return queued("repository request"), s.publish(EventCommand, c.Payload("", str(in, "requester")))
			}},
		&funcTool{name: "orgops_index", desc: "Index one repository (org/repo) or all of them ('all') into the org knowledge store.",
			params: `{"type":"object","properties":{"repo":{"type":"string"}}}`,
			run: func(ctx context.Context, in map[string]any) (any, error) {
				return queued("indexing"), s.publish(EventIndex, map[string]any{"repo": str(in, "repo")})
			}},
		&funcTool{name: "orgops_knowledge", desc: "Look up the org knowledge store: what each repository does, its stack, APIs, dependencies and conventions. Empty query lists every repository.",
			params: `{"type":"object","properties":{"query":{"type":"string"}}}`,
			run: func(ctx context.Context, in map[string]any) (any, error) {
				cards := s.Cards(str(in, "query"))
				if len(cards) == 0 {
					return "No repository cards match. Run orgops_index first if the store is empty.", nil
				}
				return cards, nil
			}},
		&funcTool{name: "orgops_audit_repos", desc: "Check every repository in the org for the org's required files and report the ones out of standard.",
			params: `{"type":"object","properties":{}}`,
			run: func(ctx context.Context, in map[string]any) (any, error) {
				res, err := s.AuditRepos(ctx)
				if err != nil {
					return nil, err
				}
				if len(res) == 0 {
					return "Every repository meets the required-files standard.", nil
				}
				return res, nil
			}},
	}
}
