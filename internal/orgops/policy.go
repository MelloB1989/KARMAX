package orgops

// Mode is how much the office may do about one kind of thing on its own.
//
//	off     — do nothing
//	notify  — tell the owners what it would do, do nothing
//	approve — prepare the action and ask; a human's "approve" carries it out
//	auto    — just do it, then say what was done
type Mode string

const (
	ModeOff     Mode = "off"
	ModeNotify  Mode = "notify"
	ModeApprove Mode = "approve"
	ModeAuto    Mode = "auto"
)

func (m Mode) valid() bool {
	switch m {
	case "", ModeOff, ModeNotify, ModeApprove, ModeAuto:
		return true
	}
	return false
}

// Action names the things a policy governs.
type Action string

const (
	ActReview        Action = "review"         // post a review on a PR
	ActApprovePR     Action = "approve_pr"     // submit an APPROVE review on a clean PR
	ActMerge         Action = "merge"          // merge a reviewed PR once CI is green
	ActRepoCreate    Action = "repo_create"    // create a repository on request
	ActIncidentFix   Action = "incident_fix"   // send a coding agent to fix an incident
	ActIncidentMerge Action = "incident_merge" // merge the incident fix once CI is green
	ActKnowledge     Action = "knowledge"      // index repositories into memory
)

// Policy is the autonomy for each action, per installation and per repository.
type Policy struct {
	Review        Mode `yaml:"review"`
	ApprovePR     Mode `yaml:"approve_pr"`
	Merge         Mode `yaml:"merge"`
	RepoCreate    Mode `yaml:"repo_create"`
	IncidentFix   Mode `yaml:"incident_fix"`
	IncidentMerge Mode `yaml:"incident_merge"`
	Knowledge     Mode `yaml:"knowledge"`
	// MaxAutoRisk caps what "auto" covers: an action whose assessed risk (0-1)
	// is above it is asked about instead. Zero means the default, 0.5; set 1 to
	// let auto mean auto regardless.
	MaxAutoRisk float64 `yaml:"max_auto_risk"`
}

// The defaults are deliberately conservative: the office reviews and alerts on
// its own, and asks before it changes anything.
func (p Policy) withDefaults() Policy {
	def := Policy{
		Review: ModeAuto, ApprovePR: ModeNotify, Merge: ModeApprove,
		RepoCreate: ModeApprove, IncidentFix: ModeApprove, IncidentMerge: ModeApprove,
		Knowledge: ModeAuto, MaxAutoRisk: 0.5,
	}
	return p.over(def)
}

// over fills p's unset fields from base.
func (p Policy) over(base Policy) Policy {
	pick := func(a, b Mode) Mode {
		if a != "" {
			return a
		}
		return b
	}
	out := Policy{
		Review:        pick(p.Review, base.Review),
		ApprovePR:     pick(p.ApprovePR, base.ApprovePR),
		Merge:         pick(p.Merge, base.Merge),
		RepoCreate:    pick(p.RepoCreate, base.RepoCreate),
		IncidentFix:   pick(p.IncidentFix, base.IncidentFix),
		IncidentMerge: pick(p.IncidentMerge, base.IncidentMerge),
		Knowledge:     pick(p.Knowledge, base.Knowledge),
		MaxAutoRisk:   p.MaxAutoRisk,
	}
	if out.MaxAutoRisk == 0 {
		out.MaxAutoRisk = base.MaxAutoRisk
	}
	return out
}

func (p Policy) modes() []Mode {
	return []Mode{p.Review, p.ApprovePR, p.Merge, p.RepoCreate, p.IncidentFix, p.IncidentMerge, p.Knowledge}
}

// ModeFor returns the configured mode for an action.
func (p Policy) ModeFor(a Action) Mode {
	switch a {
	case ActReview:
		return p.Review
	case ActApprovePR:
		return p.ApprovePR
	case ActMerge:
		return p.Merge
	case ActRepoCreate:
		return p.RepoCreate
	case ActIncidentFix:
		return p.IncidentFix
	case ActIncidentMerge:
		return p.IncidentMerge
	case ActKnowledge:
		return p.Knowledge
	}
	return ModeOff
}

// Decide is the mode to act in for an action at a given risk. "auto" above the
// risk ceiling becomes "approve": the org said hands-off for the routine case,
// not for the one that could take production down.
func (p Policy) Decide(a Action, risk float64) Mode {
	m := p.ModeFor(a)
	if m == "" {
		return ModeOff
	}
	ceiling := p.MaxAutoRisk
	if ceiling == 0 {
		ceiling = 0.5
	}
	if m == ModeAuto && risk > ceiling {
		return ModeApprove
	}
	return m
}
