package runtime

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/MelloB1989/karmax/internal/agent"
	"github.com/MelloB1989/karmax/internal/comms"
	"github.com/MelloB1989/karmax/internal/harness"
	"github.com/MelloB1989/karmax/internal/policy"
	"github.com/MelloB1989/karmax/internal/store"
	"github.com/MelloB1989/karmax/internal/tools/builtin"
	"github.com/MelloB1989/karmax/pkg/loopkit"
	"go.uber.org/zap"
)

// Owning work until it is finished.
//
// An agent turn answers a message and ends. Anything it could not finish inside
// that turn simply stopped — which is why "on it" followed by nothing was a
// recognisable failure mode rather than a rare one. A task is the other shape:
// a goal, a session that keeps its own context, and a runner that comes back to
// it until somebody can say it is done.
//
// Each round is one turn in a session dedicated to that task, so round twenty
// still remembers round one without anything replaying the transcript.

const (
	// tasksPerRound bounds a tick. A dozen tasks coming due together must not
	// become a dozen model calls at once — that is a spent window in a minute.
	tasksPerRound = 3
	// maxTaskRounds before a task is given up on. Generous: this counts rounds
	// of real work, not retries of a failure.
	maxTaskRounds = 40
	// taskStuckAfter is how long a task may sit in "working" before it is
	// assumed the process working on it died.
	taskStuckAfter = 30 * time.Minute
)

// statusLine is the marker a round ends with. Parsed rather than inferred: a
// runner guessing "sounds finished" from prose is how a half-done task gets
// closed and nobody notices.
var statusLine = regexp.MustCompile(`(?im)^\s*STATUS:\s*(working|done|blocked|failed)\b`)

// reportLine is what the operator should be told, when there is something worth
// telling them. Absent means this round was routine and they hear nothing.
var reportLine = regexp.MustCompile(`(?ims)^\s*REPORT:\s*(.+?)\s*$`)

// startTaskRunner drives owned work forward on a timer.
//
// A ticker rather than a goroutine per task: timers die with the process, and
// the entire point of a task is that it survives one.
func (rt *KarmaxRuntime) startTaskRunner(ctx context.Context) {
	if rt.harness == nil {
		rt.log.Info("task runner: not started — the harness is the only engine that can work a task")
		return
	}
	kick := rt.taskKicks()
	go func() {
		rt.releaseInterruptedTasks()
		// Results recorded as owed before the daemon went down are delivered
		// first; then what was due runs now, not a tick later.
		rt.deliverOwedResults()
		rt.runDueTasks(ctx)

		t := time.NewTicker(90 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				rt.runDueTasks(ctx)
			case <-kick:
				// A request just became a task: its requester was told "on it",
				// and the work should not wait for the next tick to start.
				rt.runDueTasks(ctx)
			}
		}
	}()
}

// releaseInterruptedTasks makes every task this process did not claim due
// again. At start, anything marked working belongs to a process that is gone
// — however recently its round began — and its session resumes from its own
// transcript on the next round.
func (rt *KarmaxRuntime) releaseInterruptedTasks() {
	cutoff := rt.startedAt
	if cutoff.IsZero() {
		cutoff = time.Now().Add(-taskStuckAfter)
	}
	if n, err := rt.store.ReleaseStuckTasks(cutoff); err != nil {
		rt.log.Warn("could not release tasks the daemon died during", zap.Error(err))
	} else if n > 0 {
		rt.log.Info("released tasks the daemon died during", zap.Int64("count", n))
	}
}

// taskKicks is the channel that wakes the runner early.
func (rt *KarmaxRuntime) taskKicks() chan struct{} {
	rt.taskKickOnce.Do(func() { rt.taskKick = make(chan struct{}, 1) })
	return rt.taskKick
}

// kickTasks wakes the runner now. Never blocks: a wake already pending
// covers this one.
func (rt *KarmaxRuntime) kickTasks() {
	select {
	case rt.taskKicks() <- struct{}{}:
	default:
	}
}

func (rt *KarmaxRuntime) runDueTasks(ctx context.Context) {
	due, err := rt.store.DueTasks(time.Now(), tasksPerRound)
	if err != nil {
		rt.log.Warn("could not read due tasks", zap.Error(err))
		return
	}
	for _, task := range due {
		select {
		case <-ctx.Done():
			return
		default:
		}
		rt.runTaskRound(ctx, task)
	}
}

// runTaskRound gives one task one turn of attention.
func (rt *KarmaxRuntime) runTaskRound(ctx context.Context, task store.Task) {
	if task.Attempts >= maxTaskRounds {
		rt.log.Error("giving up on a task that will not finish",
			zap.String("task", task.ID), zap.String("title", task.Title),
			zap.Int("rounds", task.Attempts))
		text := fmt.Sprintf("I could not finish this after %d rounds and have stopped: %s",
			task.Attempts, task.Title)
		update := store.TaskUpdate{
			Status:    store.TaskFailed,
			LastError: fmt.Sprintf("no progress after %d rounds", task.Attempts),
		}
		if task.RequestID != "" {
			rt.completeRequestRound(task, update, store.TaskFailed, text+"\n\n"+firstLines(task.Progress, 6), nil)
			return
		}
		_ = rt.store.UpdateTask(task.ID, update)
		rt.reportTask(task, store.TaskFailed, text)
		return
	}

	// Claimed before the work starts. Two ticks overlapping on one long round
	// would run the same task twice, in the same session, interleaving turns.
	next := time.Now().Add(taskStuckAfter)
	if err := rt.store.UpdateTask(task.ID, store.TaskUpdate{
		Status: store.TaskWorking, NextActionAt: &next, BumpAttempt: true,
	}); err != nil {
		rt.log.Warn("could not claim a task", zap.String("task", task.ID), zap.Error(err))
		return
	}

	turnCtx, cancel := context.WithTimeout(ctx, 15*time.Minute)
	defer cancel()

	turn, err := rt.harness.SendWith(turnCtx, task.SessionKey, "task",
		taskPrompt(task), harness.Options{
			Workdir:      task.Workdir,
			Instructions: taskBrief(task, rt.policy),
			// A round whose time ran out may still finish. What it did then is
			// where the next round starts, not lost.
			OnLate: func(t harness.Turn) {
				rt.appendLate(task.ID, "The last round finished after its time ran out", agentTurn(t))
			},
		})
	if err != nil {
		// Quota, a dead process, a timeout. None of them mean the task is over
		// — they mean not now. Backed off and picked up again.
		retryAt := time.Now().Add(taskBackoff(task.Attempts))
		rt.log.Warn("a task round did not run; it will be picked up again",
			zap.String("task", task.ID), zap.Time("at", retryAt), zap.Error(err))
		_ = rt.store.UpdateTask(task.ID, store.TaskUpdate{
			Status: store.TaskWorking, LastError: err.Error(), NextActionAt: &retryAt,
		})
		return
	}

	status, report, progress := parseTaskTurn(turn.Text)
	files := parseTaskFiles(turn.Text)
	rt.log.Info("task round finished",
		zap.String("task", task.ID), zap.String("status", status),
		zap.Int("round", task.Attempts+1), zap.Bool("reporting", report != ""))

	// What was added to the task while the round ran — the rest of the turn
	// it was taken over from, arriving late — is kept for the next round.
	if cur, found, _ := rt.store.GetTask(task.ID); found {
		if extra, ok := strings.CutPrefix(cur.Progress, task.Progress); ok && strings.TrimSpace(extra) != "" {
			progress = strings.TrimSpace(progress + "\n\n" + strings.TrimSpace(extra))
		}
	}

	update := store.TaskUpdate{Status: status, Progress: progress}
	if status == store.TaskWorking {
		// Soon, but not instantly: a task that spins on nothing must not
		// occupy the runner.
		at := time.Now().Add(2 * time.Minute)
		update.NextActionAt = &at
	}
	// Blocked, done and failed all stop the clock. Blocked comes back when the
	// operator answers, which arrives as an ordinary message.

	// Every round offers its outcome; whether the operator hears it is Jev's call.
	text := report
	if text == "" {
		switch status {
		case store.TaskDone:
			text = "Done: " + task.Title
		case store.TaskFailed:
			text = "I could not finish this: " + task.Title + "\n\n" + firstLines(progress, 6)
		default:
			text = clip(strings.TrimSpace(progress), 500)
		}
	}

	// A request's outcome is owed to whoever asked, not offered: it is
	// recorded with the round and delivered once (completeRequestRound).
	if task.RequestID != "" && status != store.TaskWorking {
		rt.completeRequestRound(task, update, status, text, files)
		return
	}

	if err := rt.store.UpdateTask(task.ID, update); err != nil {
		rt.log.Warn("could not record a task round", zap.String("task", task.ID), zap.Error(err))
	}
	rt.reportTask(task, status, text)
}

// completeRequestRound records a request task's finished, failed or blocked
// round together with what it owes the requester, then delivers it. The
// result and every file are written as owed in the same transaction as the
// round, so a crash anywhere after this point is finished by
// deliverOwedResults on the next start — and the ledger keeps a result that
// already went out from going out again.
func (rt *KarmaxRuntime) completeRequestRound(task store.Task, update store.TaskUpdate, status, text string, files []string) {
	round := task.Attempts + 1
	if status == store.TaskBlocked {
		text = "I need something from you to carry on with \"" + task.Title + "\":\n\n" + text
	}
	owed := []store.Delivery{{Key: fmt.Sprintf("task:%s:%d:00", task.ID, round),
		ChannelID: task.ChannelID, Target: task.Target, Content: strings.TrimSpace(text)}}
	for i, path := range files {
		owed = append(owed, store.Delivery{Key: fmt.Sprintf("task:%s:%d:%02d", task.ID, round, i+1),
			ChannelID: task.ChannelID, Target: task.Target, Content: comms.FileMessage(path), FilePath: path})
	}
	update.Reported = strings.TrimSpace(text)
	if err := rt.store.CompleteTaskRound(task.ID, update, owed); err != nil {
		// Not recorded, so not delivered either: the task is still claimed, and
		// is picked up again when its claim lapses.
		rt.log.Error("could not record a request task's outcome", zap.String("task", task.ID), zap.Error(err))
		return
	}
	rt.log.Info("a request task finished a round its requester is owed",
		zap.String("task", task.ID), zap.String("request", task.RequestID), zap.String("status", status),
		zap.Int("files", len(files)))
	rt.taskHooks.markReported(task.ID)
	rt.deliverOwedResults()
}

// deliverOwedResults sends every task result recorded as owed and not yet
// delivered: just after a round, and at start for what a crash cut off.
func (rt *KarmaxRuntime) deliverOwedResults() {
	rt.owedMu.Lock()
	defer rt.owedMu.Unlock()
	owed, err := rt.store.OwedDeliveries("task:")
	if err != nil {
		rt.log.Warn("could not read the results owed to requesters", zap.Error(err))
		return
	}
	for _, d := range owed {
		if err := rt.deliverOwed(d); err != nil {
			rt.log.Warn("could not deliver a task's result; it stays owed", zap.String("key", d.Key), zap.Error(err))
		}
	}
}

// deliverOwed sends one owed result where its request came from: the chat
// it was asked in, or the operator in the app.
func (rt *KarmaxRuntime) deliverOwed(d store.Delivery) error {
	if d.ChannelID == "" || d.Target == "" {
		title := "Task update"
		if parts := strings.SplitN(d.Key, ":", 3); len(parts) >= 2 {
			if t, found, _ := rt.store.GetTask(parts[1]); found {
				title = t.Title
			}
		}
		body := d.Content
		if d.FilePath != "" {
			body = "File ready: " + d.FilePath
		}
		_, err := rt.deliverWith(d, func() error {
			builtin.PushAppNotification(rt.store, rt.loopDefaultAgent, "info", title, body)
			return nil
		})
		return err
	}
	if d.FilePath != "" {
		_, err := rt.deliverWith(d, func() error { return rt.sendFileTo(d.ChannelID, d.Target, d.FilePath) })
		return err
	}
	_, err := rt.deliverWith(d, func() error { return rt.sendFrom(comms.ProducerLoop, d.ChannelID, d.Target, d.Content) })
	return err
}

// appendLate gives a task what a turn of its, or the turn it was taken over
// from, said and did after its caller stopped waiting.
func (rt *KarmaxRuntime) appendLate(taskID, heading string, t agent.HarnessTurn) {
	text := strings.TrimSpace(t.Text)
	var did []string
	for _, tc := range t.ToolCalls {
		if !tc.Failed {
			did = append(did, tc.Name)
		}
	}
	if text == "" && len(did) == 0 {
		return
	}
	note := "\n\n## " + heading + "\n\n"
	if text != "" {
		note += clip(text, 2000) + "\n"
	}
	if len(did) > 0 {
		note += "It ran: " + strings.Join(did, ", ") + "\n"
	}
	note += "Check whether that already did the work before doing it again."
	if err := rt.appendTaskProgress(taskID, note); err != nil {
		rt.log.Warn("could not give a task what a late turn did", zap.String("task", taskID), zap.Error(err))
		return
	}
	rt.log.Info("a late turn's result went to its task", zap.String("task", taskID), zap.Int("len", len(text)))
}

// agentTurn is a harness turn in the agent's terms.
func agentTurn(t harness.Turn) agent.HarnessTurn {
	calls := make([]agent.HarnessToolCall, 0, len(t.ToolCalls))
	for _, tc := range t.ToolCalls {
		calls = append(calls, agent.HarnessToolCall{Name: tc.Name, Input: tc.Input, Failed: tc.Failed})
	}
	return agent.HarnessTurn{Available: true, Text: t.Text, ToolCalls: calls}
}

// fileLine is how a round names a file its requester should receive.
var fileLine = regexp.MustCompile(`(?im)^\s*FILE:\s*(\S.*?)\s*$`)

// parseTaskFiles reads the files a round names for delivery: absolute paths
// to files that exist.
func parseTaskFiles(text string) []string {
	var out []string
	seen := map[string]bool{}
	for _, m := range fileLine.FindAllStringSubmatch(text, -1) {
		p := strings.Trim(strings.TrimSpace(m[1]), "`'\"")
		if !filepath.IsAbs(p) || seen[p] {
			continue
		}
		if st, err := os.Stat(p); err != nil || st.IsDir() {
			continue
		}
		seen[p] = true
		out = append(out, p)
	}
	return out
}

// reportTask tells the operator, on the channel the task came from, if Jev
// judges the update worth sending now.
func (rt *KarmaxRuntime) reportTask(task store.Task, status, text string) {
	text = strings.TrimSpace(text)
	if text == "" {
		return
	}
	if !rt.worthTelling(task, status, text) {
		rt.log.Info("task update held back", zap.String("task", task.ID), zap.String("status", status))
		return
	}
	if rt.deliverTaskUpdate(task, status, text) {
		rt.taskHooks.markReported(task.ID)
		if rt.store != nil {
			_ = rt.store.UpdateTask(task.ID, store.TaskUpdate{
				Reported: text, KeepLastError: true, KeepNextActionAt: true,
			})
		}
	}
}

func (rt *KarmaxRuntime) deliverTaskUpdate(task store.Task, status, text string) bool {
	if task.ChannelID != "" && task.Target != "" {
		if err := rt.sendTaskText(task.ChannelID, task.Target, text); err == nil {
			return true
		} else {
			rt.log.Warn("could not report a task to its channel",
				zap.String("task", task.ID), zap.Error(err))
		}
	}
	// No channel, or the send failed. The app feed is the floor: an update the
	// operator never receives is the same as not having done the work.
	kind := "info"
	if status == store.TaskFailed || status == store.TaskBlocked {
		kind = "alert"
	}
	builtin.PushAppNotification(rt.store, task.AgentID, kind, task.Title, text)
	return true
}

// worthTelling asks Jev; an unavailable Jev means yes.
func (rt *KarmaxRuntime) worthTelling(task store.Task, status, text string) bool {
	since := rt.taskHooks.sinceReport(task)
	state := map[string]any{
		"goal":                      clip(strings.TrimSpace(task.Goal), 600),
		"status":                    status,
		"latest_update":             clip(strings.TrimSpace(text), 800),
		"last_told_operator":        clip(strings.TrimSpace(task.Reported), 400),
		"minutes_since_last_report": int(since.Minutes()),
		"finished":                  status == store.TaskDone || status == store.TaskFailed,
		"failed":                    status == store.TaskFailed,
		"blocked":                   status == store.TaskBlocked,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	d, err := rt.taskDecide(ctx, state, loopkit.Questions{
		"send_update": loopkit.Noul("An assistant is working a long task for its operator. Should the operator be sent " +
			"this update right now? Yes for an outcome (finished or failed), a blocker or question, or progress they " +
			"would want to know about. No for routine progress, anything repeating what they were last told, or an " +
			"update that comes too soon after the last one to add anything."),
	})
	if err != nil {
		rt.log.Info("task update gate unavailable; sending", zap.Error(err))
		return true
	}
	return d.Yes("send_update", 0.5)
}

func (rt *KarmaxRuntime) taskDecide(ctx context.Context, state any, qs loopkit.Questions) (*loopkit.Decision, error) {
	if rt.taskHooks.decide != nil {
		return rt.taskHooks.decide(ctx, state, qs)
	}
	return rt.decide(ctx, state, qs)
}

func (rt *KarmaxRuntime) sendTaskText(channelID, target, text string) error {
	if rt.taskHooks.send != nil {
		return rt.taskHooks.send(channelID, target, text)
	}
	if rt.comms == nil {
		return fmt.Errorf("no comms")
	}
	return rt.comms.SendFrom(comms.ProducerLoop, channelID, target, text)
}

// taskHooks are the runner's seams: the Jev call and the channel send.
type taskHooks struct {
	decide   func(ctx context.Context, state any, qs loopkit.Questions) (*loopkit.Decision, error)
	send     func(channelID, target, text string) error
	sendFile func(channelID, target, path string) error

	mu       sync.Mutex
	lastSent map[string]time.Time
}

func (h *taskHooks) markReported(id string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.lastSent == nil {
		h.lastSent = map[string]time.Time{}
	}
	h.lastSent[id] = time.Now()
}

func (h *taskHooks) sinceReport(t store.Task) time.Duration {
	h.mu.Lock()
	defer h.mu.Unlock()
	if at, ok := h.lastSent[t.ID]; ok {
		return time.Since(at)
	}
	return time.Since(t.CreatedAt)
}

// taskPrompt is what one round asks.
func taskPrompt(t store.Task) string {
	var b strings.Builder
	b.WriteString("Continue this task.\n\n## The task\n\n")
	b.WriteString(t.Goal)
	if strings.TrimSpace(t.Progress) != "" {
		b.WriteString("\n\n## Where you left off\n\n")
		b.WriteString(t.Progress)
	}
	if strings.TrimSpace(t.LastError) != "" {
		b.WriteString("\n\n## The last round did not complete\n\n")
		b.WriteString(t.LastError)
		b.WriteString("\n\nThat is a reason to try another way, not a reason to stop.")
	}
	b.WriteString("\n\n## This round\n\nDo the next real piece of work now — actually run the " +
		"commands, make the changes, send the messages. Then end your reply with:\n\n" +
		"STATUS: working | done | blocked | failed\n" +
		"REPORT: <one or two sentences> — ONLY when the operator needs to know something now.\n\n" +
		"Use `done` only when the whole task is finished and you have verified it. Use `blocked` " +
		"only when you genuinely cannot proceed without an answer from the operator, and say what " +
		"you need in REPORT. Anything else is `working`.")
	if t.RequestID != "" {
		b.WriteString("\n\nSomebody is waiting for this (see your brief). When you end the task — done, " +
			"failed or blocked — your REPORT is sent to them exactly as you write it, so make it the " +
			"answer itself, not a note about it. Name each file they should receive on its own line, " +
			"`FILE: /absolute/path`, and KARMAX sends it with the REPORT. Do not send the result to them " +
			"yourself — not with wacli, comms.send or anything else — or they get it twice.")
	}
	return b.String()
}

// taskBrief is the standing instruction the task's session is seeded with.
//
// Seeded once, then owned by the session — so what it learns about this
// particular job accumulates in that file instead of being re-stated every
// round.
func taskBrief(t store.Task, p policy.Policy) string {
	return taskBriefBase(t, p) + requestBrief(t)
}

// requestBrief is what a task that continues a request adds to its brief:
// who is waiting and what the conversation knew when it was handed over.
func requestBrief(t store.Task) string {
	if t.RequestID == "" {
		return ""
	}
	var b strings.Builder
	b.WriteString(`
## Somebody is waiting for this

This task continues a request that could not be finished in the turn that
received it. The person who asked was told it is underway, and is waiting for
the result. KARMAX delivers it to them — your final REPORT, and every file you
name with a FILE: line — exactly once, even across restarts. So do not send the
result to them yourself, whatever the original request says about replying:
that is what KARMAX's delivery does. Messages to other people that are part of
the work itself are yours to send as usual.

Other passes over the same chat are told this task owns the request, so you
are its only owner: nothing else will answer it, and you must not leave it
unanswered. If you cannot finish, end STATUS: failed (or blocked, with what you
need) and say plainly in REPORT what was done and what was not.
`)
	if ctx := strings.TrimSpace(t.Context); ctx != "" {
		b.WriteString("\n## What the conversation knew when it was handed over\n\n")
		b.WriteString(ctx)
		b.WriteString("\n")
	}
	return b.String()
}

// taskBriefBase is the brief every task gets.
func taskBriefBase(t store.Task, p policy.Policy) string {
	return `# A task you own

The operator handed you this and expects it finished. You are not advising them
on it; you are doing it.

    ` + firstLineOf(t.Title) + `

## How this works

You will be asked to continue this task repeatedly, in this same session, until
it is done. Everything you learn stays here between rounds — so keep notes in
this file as you go: what you have tried, what worked, what the shape of the
problem turned out to be. A round that rediscovers what round three already knew
is a wasted round.

Do one real piece of work per round and stop. Not a plan for the next five
rounds — the actual next thing, done.

## Authority

The operator has pre-authorised you to finish this by whatever means it takes,
including messaging other people as them, posting, and changing things that are
hard to change back. Do not stop to ask permission for those. Do them, and say
what you did.

One exception, the operator's hard floor: ` + strings.Join(p.HardFloor(), "; ") + `.
For those, ask first — ` + "`karmax tool call propose title=\"…\" action=\"<exact command or text>\" floor=\"<the rule>\"`" + ` —
then end the round STATUS: blocked until they answer.

The limits that remain are the ones that are not about permission:

- Do not disclose the operator's private data to a third party. That is not a
  permission question; it cannot be undone.
- Do not claim something is done unless a command in that round did it. An
  honest "blocked" always beats an invented "done".

## Being stuck

Blocked means you need an answer only the operator has — a decision, a
credential, a fact nobody wrote down. It does not mean the first approach
failed. Try another way first; you have a real shell, the whole ` + "`karmax`" + ` CLI,
and the web.
`
}

// parseTaskTurn reads the round's outcome out of the reply.
//
// A missing marker means working, not done. Getting that default wrong in the
// other direction closes tasks that are still half finished, and the operator
// finds out when the thing they asked for never arrives.
func parseTaskTurn(text string) (status, report, progress string) {
	status = store.TaskWorking
	if m := statusLine.FindStringSubmatch(text); m != nil {
		status = strings.ToLower(m[1])
	}
	if m := reportLine.FindStringSubmatch(text); m != nil {
		report = strings.TrimSpace(m[1])
	}
	// The prose minus the markers is what the next round reads as "where you
	// left off".
	progress = strings.TrimSpace(statusLine.ReplaceAllString(text, ""))
	progress = strings.TrimSpace(reportLine.ReplaceAllString(progress, ""))
	if len(progress) > 4000 {
		progress = progress[len(progress)-4000:]
	}
	return status, report, progress
}

// taskBackoff spaces out rounds that could not run at all.
func taskBackoff(attempt int) time.Duration {
	d := time.Duration(1<<uint(min(attempt, 5))) * time.Minute
	if d > 30*time.Minute {
		d = 30 * time.Minute
	}
	return d
}

func firstLineOf(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return s
}

func firstLines(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > n {
		lines = lines[:n]
	}
	return strings.Join(lines, "\n")
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
