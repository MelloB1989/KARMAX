package runtime

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/MelloB1989/karmax/internal/harness"
	"github.com/MelloB1989/karmax/internal/store"
	"github.com/MelloB1989/karmax/internal/tools/builtin"
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
	go func() {
		// Anything left "working" belongs to a process that is gone.
		if n, err := rt.store.ReleaseStuckTasks(time.Now().Add(-taskStuckAfter)); err != nil {
			rt.log.Warn("could not release tasks the daemon died during", zap.Error(err))
		} else if n > 0 {
			rt.log.Info("released tasks the daemon died during", zap.Int64("count", n))
		}

		t := time.NewTicker(90 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				rt.runDueTasks(ctx)
			}
		}
	}()
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
		_ = rt.store.UpdateTask(task.ID, store.TaskUpdate{
			Status:    store.TaskFailed,
			LastError: fmt.Sprintf("no progress after %d rounds", task.Attempts),
		})
		rt.reportTask(task, store.TaskFailed,
			fmt.Sprintf("I could not finish this after %d rounds and have stopped: %s",
				task.Attempts, task.Title))
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
			Instructions: taskBrief(task),
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
	rt.log.Info("task round finished",
		zap.String("task", task.ID), zap.String("status", status),
		zap.Int("round", task.Attempts+1), zap.Bool("reporting", report != ""))

	update := store.TaskUpdate{Status: status, Progress: progress}
	if status == store.TaskWorking {
		// Soon, but not instantly: a task that spins on nothing must not
		// occupy the runner.
		at := time.Now().Add(2 * time.Minute)
		update.NextActionAt = &at
	}
	// Blocked, done and failed all stop the clock. Blocked comes back when the
	// operator answers, which arrives as an ordinary message.

	if report != "" {
		update.Reported = report
	}
	if err := rt.store.UpdateTask(task.ID, update); err != nil {
		rt.log.Warn("could not record a task round", zap.String("task", task.ID), zap.Error(err))
	}

	// Told when there is something to tell, or when the work is over either
	// way. A round that merely made progress says nothing: twenty rounds of
	// "still going" is how an assistant becomes noise to be muted.
	switch {
	case report != "" && report != task.Reported:
		rt.reportTask(task, status, report)
	case status == store.TaskDone:
		rt.reportTask(task, status, "Done: "+task.Title)
	case status == store.TaskFailed:
		rt.reportTask(task, status, "I could not finish this: "+task.Title+
			"\n\n"+firstLines(progress, 6))
	}
}

// reportTask tells the operator, on the channel the task came from.
func (rt *KarmaxRuntime) reportTask(task store.Task, status, text string) {
	text = strings.TrimSpace(text)
	if text == "" {
		return
	}
	if task.ChannelID != "" && task.Target != "" && rt.comms != nil {
		if err := rt.comms.Send(task.ChannelID, task.Target, text); err != nil {
			rt.log.Warn("could not report a task to its channel",
				zap.String("task", task.ID), zap.Error(err))
		} else {
			return
		}
	}
	// No channel, or the send failed. The app feed is the floor: an update the
	// operator never receives is the same as not having done the work.
	kind := "info"
	if status == store.TaskFailed || status == store.TaskBlocked {
		kind = "alert"
	}
	builtin.PushAppNotification(rt.store, task.AgentID, kind, task.Title, text)
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
	return b.String()
}

// taskBrief is the standing instruction the task's session is seeded with.
//
// Seeded once, then owned by the session — so what it learns about this
// particular job accumulates in that file instead of being re-stated every
// round.
func taskBrief(t store.Task) string {
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
including actions that cost money, post publicly, message other people, or
change things that are hard to change back. Do not stop to ask permission for
those. Do them, and say what you did.

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
