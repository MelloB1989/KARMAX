package daemon

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/MelloB1989/karmax/fleet/internal/identity"
	"github.com/MelloB1989/karmax/fleet/internal/ledger"
	"github.com/MelloB1989/karmax/fleet/internal/reconcile"
)

// ErrBadInput is a run the caller got wrong: no brief, a bad repo, an unknown
// agent. The server answers 400.
var ErrBadInput = errors.New("bad input")

// Candidate is one agent as Pick weighed it.
type Candidate struct {
	Agent    string  `json:"agent"`
	Eligible bool    `json:"eligible"`
	Why      string  `json:"why"`
	FiveHour float64 `json:"five_hour_pct"`
	SevenDay float64 `json:"seven_day_pct"`
	IdleS    int64   `json:"idle_s"`
}

// PickReply is Pick's answer: the chosen agent ("" when none is eligible)
// and every agent with the reason it was or was not chosen, chosen first.
type PickReply struct {
	Agent      string      `json:"agent"`
	Why        string      `json:"why"`
	Candidates []Candidate `json:"candidates"`
}

// NoFreeAgent is a pick that found nobody. It carries what Pick saw.
type NoFreeAgent struct{ Candidates []Candidate }

func (e *NoFreeAgent) Error() string {
	var why []string
	for _, c := range e.Candidates {
		why = append(why, c.Agent+": "+c.Why)
	}
	return "no free agent (" + strings.Join(why, "; ") + ")"
}

func (e *NoFreeAgent) Unwrap() error { return ErrConflict }

// Pick chooses the worker a new task should go to, with no regard to who
// gives it or the model it wants (PickFor).
func (f *Fleet) Pick(repo string, now time.Time) PickReply { return f.PickFor(repo, "", "", now) }

// PickModel is Pick for a task that wants a model: only workers whose
// agent's models: list allows it are eligible.
func (f *Fleet) PickModel(repo, model string, now time.Time) PickReply {
	return f.PickFor(repo, "", model, now)
}

// PickFor chooses the worker a task from `from` that wants `model` should go
// to. Eligible: not disabled, down, cooling, working, holding an open task,
// low or reserved; its agent's models: list allows the model; and, for a
// task with a known side, no sibling slot holds a task from the other side
// (OperatorAssigner). Best first: a worker on an agent with no busy slot (tasks
// spread over containers before they share one), then the agent whose last
// task was in the same repo, then the lowest 5 h quota use, the lowest 7 d,
// the longest idle.
func (f *Fleet) PickFor(repo, from, model string, now time.Time) PickReply {
	return f.pickAmong(repo, from, model, nil, now)
}

// notLent is why a pick passes over a sandbox: it is lent to guests, and
// takes another's task only when it is named.
const notLent = "a sandbox, lent to guests: name it with --agent to give it a task"

// pickAmong is PickFor for a run that may go only to the agents among (a
// guest's lent ones): a sandbox is eligible only when it is among them.
func (f *Fleet) pickAmong(repo, from, model string, among []string, now time.Time) PickReply {
	rows := f.Status(now)
	type scored struct {
		c        Candidate
		busy     int // the agent's other slots that hold a task or work
		affinity bool
	}
	held := map[string][]string{} // agent → the assigners of its slots' open tasks
	busy := map[string]int{}
	for _, r := range rows {
		if as := f.openAssigners(r); len(as) > 0 {
			held[r.SlotOf] = append(held[r.SlotOf], as...)
			busy[r.SlotOf]++
		} else if r.State == string(reconcile.Working) {
			busy[r.SlotOf]++
		}
	}
	var all []scored
	for _, r := range rows {
		c := Candidate{Agent: r.Agent, FiveHour: r.FiveHour, SevenDay: r.SevenDay, IdleS: r.IdleS}
		c.Why = f.ineligible(r)
		if c.Why == "" && f.worker(r.Agent).Sandbox && !slices.Contains(among, r.SlotOf) {
			c.Why = notLent
		}
		if models := f.worker(r.Agent).Models; c.Why == "" && model != "" && !ModelAllowed(models, model) {
			c.Why = fmt.Sprintf("does not list %s (models: %s)", model, strings.Join(models, ", "))
		}
		if c.Why == "" && from != "" {
			c.Why = f.otherSide(from, held[r.SlotOf])
		}
		c.Eligible = c.Why == ""
		s := scored{c: c, busy: busy[r.SlotOf]}
		if c.Eligible && repo != "" {
			s.affinity = f.lastRepo(r.SlotOf) == repo
		}
		all = append(all, s)
	}
	sort.SliceStable(all, func(i, j int) bool {
		a, b := all[i], all[j]
		switch {
		case a.c.Eligible != b.c.Eligible:
			return a.c.Eligible
		case !a.c.Eligible:
			return a.c.Agent < b.c.Agent
		case (a.busy == 0) != (b.busy == 0):
			return a.busy == 0
		case a.affinity != b.affinity:
			return a.affinity
		case a.c.FiveHour != b.c.FiveHour:
			return a.c.FiveHour < b.c.FiveHour
		case a.c.SevenDay != b.c.SevenDay:
			return a.c.SevenDay < b.c.SevenDay
		case a.c.IdleS != b.c.IdleS:
			return a.c.IdleS > b.c.IdleS
		}
		return a.c.Agent < b.c.Agent
	})
	reply := PickReply{Candidates: make([]Candidate, len(all))}
	for i, s := range all {
		reply.Candidates[i] = s.c
	}
	if len(all) == 0 || !all[0].c.Eligible {
		return reply
	}
	top := all[0]
	reply.Agent = top.c.Agent
	why := fmt.Sprintf("5h %.0f%%, 7d %.0f%%, idle %s", top.c.FiveHour, top.c.SevenDay, Dur(time.Duration(top.c.IdleS)*time.Second))
	if top.affinity {
		why = "last task was in " + repo + "; " + why
	}
	if top.busy > 0 {
		why = fmt.Sprintf("every free agent has a busy slot; %s", why)
	}
	reply.Why = why
	reply.Candidates[0].Why = why
	for i := 1; i < len(reply.Candidates); i++ {
		if reply.Candidates[i].Eligible {
			reply.Candidates[i].Why = "eligible, ranked below " + top.c.Agent
		}
	}
	return reply
}

// openAssigners is who gave a worker the tasks it holds open: its current
// one and the ledger's.
func (f *Fleet) openAssigners(r Row) []string {
	var out []string
	open, _ := f.db.Tasks(true, r.Agent, 0)
	for _, t := range open {
		out = append(out, t.Assigner)
	}
	if r.Task != "" && len(open) == 0 {
		if t, err := f.db.Task(r.Task); err == nil {
			out = append(out, t.Assigner)
		} else {
			out = append(out, "") // assigned since: its side is unknown, so it is neither
		}
	}
	return out
}

// otherSide is why a task from `from` may not share a container with the
// tasks its slots hold: one container has one key and one /work for all its
// slots, so slots are no boundary between a member's task and the
// operator's. "" when none of them is from the other side.
func (f *Fleet) otherSide(from string, held []string) string {
	mine := f.OperatorAssigner(from)
	for _, a := range held {
		if a == "" || f.OperatorAssigner(a) == mine {
			continue
		}
		if mine {
			return "a sibling slot holds a member's task (" + a + "); slots share a container, so an operator's task waits for another agent"
		}
		return "a sibling slot holds an operator's task; slots share a container, so a member's task waits for another agent"
	}
	return ""
}

// lastRepo is the repo of the agent's last task, in any of its slots: they
// share its container's worktrees.
func (f *Fleet) lastRepo(agent string) string {
	var last string
	var at time.Time
	for _, w := range append([]string{agent}, f.cfg.Siblings(agent)...) {
		ts, err := f.db.Tasks(false, w, 1)
		if err != nil || len(ts) == 0 || ts[0].Repo == "" || !ts[0].Assigned.After(at) {
			continue
		}
		last, at = ts[0].Repo, ts[0].Assigned
	}
	return last
}

// ineligible is why an agent cannot take a task now, "" when it can. The
// phase alone is not trusted: an agent can read standby between tool calls
// of a task it still holds, so an open task — in its state or in the ledger —
// excludes it too.
func (f *Fleet) ineligible(r Row) string {
	switch reconcile.Phase(r.State) {
	case reconcile.Disabled:
		return "disabled (" + r.Reason + ")"
	case reconcile.Down:
		return "down"
	case reconcile.CodeMissing:
		return "code directory missing"
	case reconcile.Cooling:
		return "cooling"
	case reconcile.Unreachable:
		return "its host is out of reach"
	case reconcile.Working:
		if r.Task != "" {
			return "working on " + r.Task
		}
		return "working"
	}
	if r.Unreachable {
		return "its host is not answering"
	}
	if r.Rotating {
		return "rotating to a fresh session" // a task now would run in the last one's, with its context
	}
	if r.Session == "" && !r.RecordedAt.IsZero() {
		return "starting a session" // rotated: a brief now would find no one to take it
	}
	if r.Task != "" {
		return "holds open task " + r.Task
	}
	if open, err := f.db.Tasks(true, r.Agent, 1); err != nil {
		return "ledger: " + err.Error()
	} else if len(open) > 0 {
		return "holds open task " + open[0].ID
	}
	if r.Low {
		return "low on quota"
	}
	if r.Reserved {
		return "quota reserved"
	}
	return ""
}

// RunRequest is a task to hand to the fleet. JSON: brief, repo, agent, from,
// task, queue ("30m"), model.
type RunRequest struct {
	Brief, Repo, Agent, From, Task string
	// Model is the model the task's session runs; empty: the agent's own.
	// When the task closes and the session rotates, the agent goes back.
	Model string
	Queue time.Duration
	// Limit is how many open tasks From may hold at once, this one
	// included; 0 is no limit. The server sets it for a tailnet guest.
	Limit int
	// AdminWork is a run with the full scope (the full token or an admin
	// device): its task may use the operator's data, unless its agent is a
	// sandbox or it is run for a login (ledger.Task.AdminWork, adminWork).
	AdminWork bool `json:"-"`
	// Attach says files are about to be copied into the task's inbox: the
	// task is placed, but its brief waits for Brief.
	Attach bool
	// Among, when set, is the only agents the run may go to: a guest's lent
	// ones. The server sets it for a guest.
	Among []string
}

// RunReply is where a run went.
type RunReply struct {
	Task  string `json:"task"`
	Agent string `json:"agent"`
	Why   string `json:"why"`
	// placed: the task was already open on Agent — this run is a retry of
	// one whose answer was lost — and nothing was assigned or sent.
	placed bool
}

// Run hands a brief to a free agent: pick (or check the named agent), make
// the task id, record the task, deliver the brief. Pick and assign are one
// critical section, so two runs never get the same agent.
func (f *Fleet) Run(ctx context.Context, in RunRequest, now time.Time) (RunReply, error) {
	if strings.TrimSpace(in.Brief) == "" {
		return RunReply{}, fmt.Errorf("%w: run needs a brief", ErrBadInput)
	}
	if in.Attach && !inboxName.MatchString(in.Task) {
		return RunReply{}, fmt.Errorf("%w: --attach needs a task id of letters, digits, '.', '_' and '-' (it names the inbox directory)", ErrBadInput)
	}
	if strings.Contains(in.Repo, "..") || strings.HasPrefix(in.Repo, "/") {
		return RunReply{}, fmt.Errorf("%w: repo %q: want a name relative to the code directory", ErrBadInput, in.Repo)
	}
	if in.Agent != "" {
		if err := f.known(in.Agent); err != nil {
			return RunReply{}, fmt.Errorf("%w: %v", ErrBadInput, err)
		}
	}
	if in.Model != "" {
		var err error
		if in.Model, err = NormalizeModel(in.Model); err != nil {
			return RunReply{}, err
		}
	}
	if in.From == "" {
		in.From = "operator"
	}
	start := time.Now()
	deadline := start.Add(in.Queue)
	for {
		rep, err := f.place(in, now.Add(time.Since(start)))
		if err == nil && rep.placed {
			return rep, nil // a retry of a run that landed: its brief is the agent's already
		}
		if err == nil && in.Attach {
			return rep, nil
		}
		if err == nil {
			return f.deliver(ctx, in, rep, now)
		}
		if !errors.Is(err, ErrConflict) || !time.Now().Add(f.waitEvery).Before(deadline) {
			return RunReply{}, err
		}
		select {
		case <-ctx.Done():
			return RunReply{}, ctx.Err()
		case <-time.After(f.waitEvery):
		}
	}
}

// place picks and assigns under runMu.
func (f *Fleet) place(in RunRequest, now time.Time) (RunReply, error) {
	f.runMu.Lock()
	defer f.runMu.Unlock()
	if in.Task != "" {
		// The same id again is the same run, retried: never a second agent.
		// A brief cut short on its way is sent again by the tick (resend).
		if t, err := f.db.Task(in.Task); err == nil {
			switch {
			case t.Closed.IsZero():
				return RunReply{Task: t.ID, Agent: t.Agent, Why: "already assigned to " + t.Agent, placed: true}, nil
			case t.Outcome != "undelivered": // its brief never got there: run it again
				return RunReply{}, fmt.Errorf("%w: task %s is closed (%s)", ErrConflict, t.ID, t.Outcome)
			}
		}
	}
	if in.Limit > 0 {
		open, err := f.db.TasksBy(true, "", ledger.OwnerOf(in.From), 0) // all their sessions' count
		if err != nil {
			return RunReply{}, err
		}
		if len(open) >= in.Limit {
			ids := make([]string, len(open))
			for i, t := range open {
				ids[i] = t.ID
			}
			return RunReply{}, fmt.Errorf("%w: %s holds %d open tasks, the most one person may (%s): close one with fleetctl done --task <id>, or cancel it",
				ErrConflict, in.From, len(open), strings.Join(ids, ", "))
		}
	}
	p := f.pickAmong(in.Repo, in.From, in.Model, in.Among, now)
	if len(in.Among) > 0 {
		if in.Agent != "" && !slices.Contains(in.Among, identity.Base(in.Agent)) {
			return RunReply{}, fmt.Errorf("%w: %s is not one of your agents", ErrForbidden, in.Agent)
		}
		p = among1(p, in.Among)
	}
	rep := RunReply{Agent: p.Agent, Why: p.Why}
	if in.Agent != "" {
		rep = RunReply{Agent: in.Agent, Why: "named"}
		for _, c := range p.Candidates {
			if c.Agent == in.Agent && !c.Eligible && c.Why != notLent { // named: lent or not
				return RunReply{}, fmt.Errorf("%w: %s is not free: %s", ErrConflict, c.Agent, c.Why)
			}
		}
	} else if p.Agent == "" {
		return RunReply{}, &NoFreeAgent{Candidates: p.Candidates}
	}
	rep.Task = in.Task
	if rep.Task == "" {
		rep.Task = TaskID(in.Brief, now)
	}
	if err := f.assign(rep.Agent, rep.Task, in.From, now); err != nil {
		return RunReply{}, err
	}
	if err := f.db.SetTaskAdminWork(rep.Task, in.AdminWork && f.adminWork(rep.Agent, in.From)); err != nil {
		return RunReply{}, err
	}
	if err := f.db.SetTaskRepo(rep.Task, in.Repo); err != nil {
		return RunReply{}, err
	}
	if err := f.db.SetTaskModel(rep.Task, in.Model); err != nil {
		return RunReply{}, err
	}
	return rep, nil
}

func (f *Fleet) deliver(ctx context.Context, in RunRequest, rep RunReply, now time.Time) (RunReply, error) {
	if in.Model != "" {
		f.setModelNow(ctx, rep.Agent)
	}
	text := "Task " + rep.Task + " from " + in.From + "."
	if in.Repo != "" {
		text += " Repo: " + in.Repo + "."
	}
	if in.Attach {
		text += " Files sent with it are in " + Inbox(rep.Task) + "."
	}
	text += " " + in.Brief
	if err := f.tell(ctx, in.From, rep.Agent, text, rep.Task, now); err != nil {
		if cerr := f.Close(rep.Agent, rep.Task, "undelivered", "", "", now); cerr != nil {
			f.Log("run: close undelivered %s: %v", rep.Task, cerr)
		}
		return RunReply{}, err
	}
	return rep, nil
}

// TaskID is YYYYMMDD-<slug>-<4 hex>: the slug is the brief's first four
// words, lowercased to [a-z0-9] and joined by "-", at most 32 characters.
// fleetctl makes one itself, so that a run it has to send again is the same
// run.
func TaskID(brief string, now time.Time) string {
	var words []string
	for _, w := range strings.Fields(brief) {
		if len(words) == 4 {
			break
		}
		w = strings.Map(func(r rune) rune {
			switch {
			case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
				return r
			case r >= 'A' && r <= 'Z':
				return r + 'a' - 'A'
			}
			return -1
		}, w)
		if w != "" {
			words = append(words, w)
		}
	}
	slug := strings.Join(words, "-")
	if len(slug) > 32 {
		slug = strings.TrimRight(slug[:32], "-")
	}
	if slug == "" {
		slug = "task"
	}
	var b [2]byte
	_, _ = rand.Read(b[:])
	return now.Format("20060102") + "-" + slug + "-" + hex.EncodeToString(b[:])
}
