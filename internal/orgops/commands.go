package orgops

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/MelloB1989/karmax/pkg/loopkit"
)

// Command is something a person asked the office for, in any chat. The words
// are deliberately few and plain so they read the same in Slack, Google Chat,
// Discord, Teams or WhatsApp — the office has no buttons to depend on.
type Command struct {
	Verb        string // approve reject fix merge review create index status
	Code        string
	Repo        string
	Number      int
	Incident    string
	Name        string
	Template    string
	Description string
}

var (
	cmdPrefix = regexp.MustCompile(`(?i)^\s*(?:[@/]?karmax[:,]?\s+)?`)
	// Codes always carry a digit, so "approve lunch" stays conversation.
	cmdApprove = regexp.MustCompile(`(?i)^(approve|approved|lgtm)\s+([A-Z]*[0-9][A-Z0-9]*)\b`)
	cmdReject  = regexp.MustCompile(`(?i)^(reject|deny|cancel)\s+([A-Z]*[0-9][A-Z0-9]*)\b`)
	cmdFix     = regexp.MustCompile(`(?i)^fix\s+(INC-[A-Z0-9]{5})(?:\s+in\s+([\w.-]+(?:/[\w.-]+)?))?`)
	cmdPRRef   = regexp.MustCompile(`(?i)^(merge|review)\s+(?:pr\s+)?([\w.-]+(?:/[\w.-]+)?)#(\d+)`)
	cmdCreate  = regexp.MustCompile(`(?i)^(?:create|new|make)\s+(?:a\s+)?(?:new\s+)?repo(?:sitory)?\s+([\w.-]+)(?:\s+from\s+([\w./-]+))?(?:\s*[:\-–]\s*(.+))?$`)
	cmdIndex   = regexp.MustCompile(`(?i)^(?:index|reindex|learn)(?:\s+([\w./-]+))?\s*$`)
	cmdStatus  = regexp.MustCompile(`(?i)^(?:orgops\s+)?status\s*$`)
)

// ParseCommand reads a chat message as an office command. ok is false for
// ordinary conversation, which is left to the orchestrator agent.
func ParseCommand(text string) (Command, bool) {
	t := strings.TrimSpace(cmdPrefix.ReplaceAllString(strings.TrimSpace(text), ""))
	if t == "" {
		return Command{}, false
	}
	if m := cmdApprove.FindStringSubmatch(t); m != nil && len(m[2]) == 5 {
		return Command{Verb: "approve", Code: strings.ToUpper(m[2])}, true
	}
	if m := cmdReject.FindStringSubmatch(t); m != nil && len(m[2]) == 5 {
		return Command{Verb: "reject", Code: strings.ToUpper(m[2])}, true
	}
	if m := cmdFix.FindStringSubmatch(t); m != nil {
		return Command{Verb: "fix", Incident: strings.ToUpper(m[1]), Repo: m[2]}, true
	}
	if m := cmdPRRef.FindStringSubmatch(t); m != nil {
		var n int
		fmt.Sscan(m[3], &n)
		return Command{Verb: strings.ToLower(m[1]), Repo: m[2], Number: n}, true
	}
	if m := cmdCreate.FindStringSubmatch(t); m != nil {
		return Command{Verb: "create", Name: m[1], Template: m[2], Description: strings.TrimSpace(m[3])}, true
	}
	if m := cmdIndex.FindStringSubmatch(t); m != nil {
		return Command{Verb: "index", Repo: m[1]}, true
	}
	if cmdStatus.MatchString(t) {
		return Command{Verb: "status"}, true
	}
	return Command{}, false
}

// Payload is the bus form of a command.
func (c Command) Payload(replyTo, requester string) map[string]any {
	return map[string]any{
		"verb": c.Verb, "code": c.Code, "repo": c.Repo, "number": c.Number, "incident": c.Incident,
		"name": c.Name, "template": c.Template, "description": c.Description,
		"reply_to": replyTo, "requester": requester,
	}
}

// CommandFromPayload reads a command back off the bus.
func CommandFromPayload(p map[string]any) Command {
	return Command{
		Verb: str(p, "verb"), Code: strings.ToUpper(str(p, "code")), Repo: str(p, "repo"), Number: num(p, "number"),
		Incident: strings.ToUpper(str(p, "incident")), Name: str(p, "name"), Template: str(p, "template"),
		Description: str(p, "description"),
	}
}

// HandleCommand carries out a command. replyTo is where the person asked.
func (s *Service) HandleCommand(ctx context.Context, k loopkit.Kit, c Command, replyTo, requester string) error {
	if replyTo == "" {
		replyTo = s.cfg.Channels.Default
	}
	switch c.Verb {
	case "status":
		s.say(ctx, k, "", replyTo, s.Status())
	case "approve":
		return s.approve(ctx, k, c.Code, replyTo, requester)
	case "reject":
		var p Pending
		if ok, _ := s.getJSON(groupPending, c.Code, &p); !ok {
			s.say(ctx, k, "", replyTo, fmt.Sprintf("Nothing is waiting under `%s`.", c.Code))
			return nil
		}
		_ = s.kv.KVDelete(groupPending, c.Code)
		_ = k.Audit("reject", p.Repo, "rejected", p.Title+" by "+requester)
		s.say(ctx, k, p.CaseID, first(p.Channel, replyTo), fmt.Sprintf("🛑 Dropped: %s (by %s).", p.Title, first(requester, "a human")))
		if inc := str(p.Args, "incident"); inc != "" && p.Action == ActIncidentFix {
			s.updateIncident(inc, func(x *Incident) { x.State = "open" })
		}
	case "fix":
		in, ok := s.IncidentByID(c.Incident)
		if !ok {
			s.say(ctx, k, "", replyTo, fmt.Sprintf("I don't know an incident %s.", c.Incident))
			return nil
		}
		s.say(ctx, k, in.CaseID, first(in.Channel, replyTo), fmt.Sprintf("🛠️ On it — sending an agent to fix %s (asked by %s).", in.ID, first(requester, "a human")))
		return s.publish(EventFix, map[string]any{"incident": in.ID, "repo": c.Repo})
	case "merge":
		// A person asking for the merge IS the approval; CI still has to be green.
		repo := s.cfg.FullName(c.Repo)
		s.queueMerge(ctx, k, Automerge{Repo: repo, Number: c.Number, Action: ActMerge, Channel: replyTo}, ModeAuto, "", "")
	case "review":
		return s.publish(EventReview, map[string]any{"repo": s.cfg.FullName(c.Repo), "number": c.Number, "reply_to": replyTo})
	case "create":
		return s.RequestRepo(ctx, k, RepoRequest{Name: c.Name, Template: c.Template, Description: c.Description,
			Requester: requester, Channel: replyTo})
	case "index":
		return s.publish(EventIndex, map[string]any{"repo": c.Repo, "reply_to": replyTo})
	default:
		return fmt.Errorf("unknown command %q", c.Verb)
	}
	return nil
}

// approve carries out a pending action. Deleting the pending row first makes
// a double "approve" — two people, or a person and the agent — act once.
func (s *Service) approve(ctx context.Context, k loopkit.Kit, code, replyTo, requester string) error {
	var p Pending
	if ok, _ := s.getJSON(groupPending, code, &p); !ok {
		s.say(ctx, k, "", replyTo, fmt.Sprintf("Nothing is waiting under `%s` — it may already be done.", code))
		return nil
	}
	if err := s.kv.KVDelete(groupPending, code); err != nil {
		return err
	}
	who := first(requester, "a human")
	_ = k.Audit("approve", p.Repo, "approved", p.Title+" by "+who)
	ch := first(p.Channel, replyTo)
	s.say(ctx, k, p.CaseID, ch, fmt.Sprintf("👍 Approved by %s: %s", who, p.Title))
	switch p.Action {
	case ActReview:
		return s.gh.CreateReview(ctx, p.Repo, p.Number, str(p.Args, "event"), str(p.Args, "body"), nil)
	case ActMerge, ActIncidentMerge:
		s.queueMerge(ctx, k, Automerge{Repo: p.Repo, Number: p.Number, Action: p.Action, CaseID: p.CaseID,
			Channel: ch, Incident: str(p.Args, "incident")}, ModeAuto, "", "")
	case ActRepoCreate:
		var owners []string
		if o := str(p.Args, "owners"); o != "" {
			owners = strings.Split(o, ",")
		}
		_, err := s.CreateRepo(ctx, k, RepoRequest{Name: str(p.Args, "name"), Template: str(p.Args, "template"),
			Description: str(p.Args, "description"), Owners: owners, Requester: str(p.Args, "requester"), Channel: ch})
		return err
	case ActIncidentFix:
		return s.publish(EventFix, map[string]any{"incident": str(p.Args, "incident")})
	}
	return nil
}
