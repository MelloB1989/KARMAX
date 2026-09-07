package runtime

import (
	"strings"
	"testing"

	"github.com/MelloB1989/karmax/internal/store"
)

// The default when a round says nothing is WORKING, not done.
//
// Getting this wrong in the other direction is the expensive one: a task closed
// while it is still half finished looks exactly like a task that succeeded, and
// the operator only finds out when the thing they asked for never arrives.
func TestARoundWithNoMarkerKeepsWorking(t *testing.T) {
	for _, reply := range []string{
		"I ran the tests, three still fail. Fixing the first one next.",
		"",
		"Looks good to me!",
		"All done here.", // prose that SOUNDS finished is not a marker
	} {
		status, _, _ := parseTaskTurn(reply)
		if status != store.TaskWorking {
			t.Errorf("reply %q closed the task as %q", reply, status)
		}
	}
}

func TestTheStatusMarkerIsRead(t *testing.T) {
	for reply, want := range map[string]string{
		"did the thing\n\nSTATUS: done":           store.TaskDone,
		"STATUS: blocked\nREPORT: need the token": store.TaskBlocked,
		"nope\nstatus: failed":                    store.TaskFailed,
		"still going\n  STATUS:   working  ":      store.TaskWorking,
		"finished it\n\nSTATUS: done\nand a note": store.TaskDone,
	} {
		if got, _, _ := parseTaskTurn(reply); got != want {
			t.Errorf("parseTaskTurn(%q) = %q, want %q", reply, got, want)
		}
	}
}

// A report is what reaches a person, so it must be exactly what the round
// asked to send — not the whole round's prose.
func TestOnlyTheReportLineIsSentOn(t *testing.T) {
	reply := "Cloned the repo and ran the migration.\n\n" +
		"REPORT: The migration needs a password I do not have.\n" +
		"STATUS: blocked"
	status, report, progress := parseTaskTurn(reply)

	if status != store.TaskBlocked {
		t.Errorf("status = %q", status)
	}
	if report != "The migration needs a password I do not have." {
		t.Errorf("report = %q", report)
	}
	// The markers must not survive into what the next round reads back.
	if strings.Contains(progress, "STATUS:") || strings.Contains(progress, "REPORT:") {
		t.Errorf("progress still carries the markers: %q", progress)
	}
	if !strings.Contains(progress, "Cloned the repo") {
		t.Errorf("progress lost the actual work: %q", progress)
	}
}

// Most rounds say nothing. Twenty rounds of "still going" is how an assistant
// becomes noise to be muted, and then a real report goes unread.
func TestARoutineRoundReportsNothing(t *testing.T) {
	_, report, _ := parseTaskTurn("Fixed two of the three failures.\n\nSTATUS: working")
	if report != "" {
		t.Errorf("a routine round wanted to message the operator: %q", report)
	}
}

// Progress is fed back into the next round's prompt, so it cannot grow without
// bound — the prompt would eventually be nothing but its own history.
func TestProgressIsBounded(t *testing.T) {
	_, _, progress := parseTaskTurn(strings.Repeat("a very long line of output\n", 2000))
	if len(progress) > 4100 {
		t.Errorf("progress kept %d bytes; it is replayed into every later round", len(progress))
	}
}

// Backoff spaces out rounds that could not run at all, and stops climbing.
func TestBackoffClimbsThenLevelsOff(t *testing.T) {
	first := taskBackoff(1)
	later := taskBackoff(4)
	if later <= first {
		t.Errorf("backoff did not grow: %s then %s", first, later)
	}
	if capped := taskBackoff(50); capped > 30*60*1e9 {
		t.Errorf("backoff grew past its cap: %s", capped)
	}
}

// The brief is the session's whole understanding of its authority. The operator
// chose full autonomy, so it must not read as though it should stop and ask.
func TestTheBriefGrantsAutonomyAndStillForbidsTheIrreversibleLeak(t *testing.T) {
	brief := taskBrief(store.Task{Title: "ship the thing", Goal: "ship it"})
	for _, want := range []string{"pre-authorised", "Do not stop to ask permission"} {
		if !strings.Contains(brief, want) {
			t.Errorf("the brief does not grant autonomy: missing %q", want)
		}
	}
	// Autonomy over the operator's OWN work is not autonomy to leak their data
	// to somebody else, which is the one thing that cannot be undone.
	if !strings.Contains(brief, "private data") {
		t.Error("the brief dropped the disclosure limit")
	}
	if !strings.Contains(brief, "blocked") {
		t.Error("the brief does not say what being stuck means")
	}
}

// The prompt has to carry what the last round learned, or every round starts
// over and the task never converges.
func TestThePromptCarriesTheStateForward(t *testing.T) {
	p := taskPrompt(store.Task{
		Goal:      "migrate the database",
		Progress:  "the schema is copied; data is next",
		LastError: "harness turn timed out",
	})
	for _, want := range []string{
		"migrate the database",
		"the schema is copied",
		"harness turn timed out",
		"STATUS:",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("the round's prompt is missing %q", want)
		}
	}
	// A failed round must read as a reason to try differently, not to stop.
	if !strings.Contains(p, "not a reason to stop") {
		t.Error("a previous failure was presented without telling it to continue")
	}
}
