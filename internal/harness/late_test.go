package harness

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"
)

// A fake CLI whose answer to "slow" arrives after a while, from a subshell
// that outlives stdin closing — the shape of a launched (docker exec) session
// finishing its turn after the caller stopped waiting and the session was
// asked to close.
const lateScript = `while IFS= read -r line; do
  case "$line" in
    *slow*) ( sleep 0.4; ` + assistantLate + `; ` + resultLate + ` ) & ;;
    *) ` + resultOK + ` ;;
  esac
done
wait
`

const assistantLate = `echo '{"type":"assistant","message":{"model":"m","content":[{"type":"text","text":"Done: the export page is live."},{"type":"tool_use","id":"t9","name":"Bash","input":{"command":"fleetctl merge agent-03"}}]}}'`
const resultLate = `echo '{"type":"result","subtype":"success","is_error":false,"result":"Done: the export page is live.","total_cost_usd":0.2,"usage":{"input_tokens":5,"output_tokens":2}}'`

func lateSupervisor(t *testing.T, onBG func(string, string, Turn)) *Supervisor {
	t.Helper()
	sup := New(Config{
		Binary: writeScript(t, lateScript), WorkdirRoot: t.TempDir(), MaxLive: 4, Env: os.Environ(),
		Policies: map[string]Policy{"agent": {TurnTimeout: 5 * time.Second, Idle: time.Minute,
			Launch: []string{"env"}}},
		OnBackgroundTurn: onBG,
	}, newMemStore(), NewBreaker(0.95, nil), testLog{t}, nil)
	t.Cleanup(sup.Shutdown)
	return sup
}

// The caller gave up on a turn — its time ran out — and the session finished
// it anyway. What it said used to go to the bus as a background turn nobody
// read; it now reaches the caller that asked, through OnLate.
func TestTheRestOfATurnItsCallerGaveUpOnReachesThatCaller(t *testing.T) {
	bg := make(chan Turn, 4)
	sup := lateSupervisor(t, func(_, _ string, t Turn) { bg <- t })
	late := make(chan Turn, 4)

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	_, err := sup.SendWith(ctx, "agent:nexus", "agent", "slow: build the export page", Options{OnLate: func(t Turn) { late <- t }})
	if err == nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("SendWith = %v, want the caller's deadline", err)
	}

	select {
	case got := <-late:
		if got.Text != "Done: the export page is live." || !got.Late {
			t.Errorf("late turn = %+v", got)
		}
		if len(got.ToolCalls) != 1 || got.ToolCalls[0].Name != "Bash" {
			t.Errorf("late turn's tool calls = %+v", got.ToolCalls)
		}
	case <-time.After(8 * time.Second):
		t.Fatal("the rest of the turn never reached the caller that gave up on it")
	}
	select {
	case got := <-bg:
		t.Errorf("a turn its caller claimed was also handed to the background handler: %+v", got)
	case <-time.After(100 * time.Millisecond):
	}
}

// A caller that gave up without asking for the rest leaves it to the
// background handler, marked late so whoever routes it can tell.
func TestALateTurnNobodyClaimedGoesToTheBackgroundHandlerMarkedLate(t *testing.T) {
	bg := make(chan Turn, 4)
	sup := lateSupervisor(t, func(_, _ string, t Turn) { bg <- t })

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	if _, err := sup.SendWith(ctx, "agent:nexus", "agent", "slow", Options{}); err == nil {
		t.Fatal("the caller's deadline passed unnoticed")
	}
	select {
	case got := <-bg:
		if !got.Late || got.Text != "Done: the export page is live." {
			t.Errorf("background turn = %+v, want the late turn, marked late", got)
		}
	case <-time.After(8 * time.Second):
		t.Fatal("the late turn was lost")
	}
}

// A caller that gives up on a session whose process has already gone opens no
// background turn: nothing more will be said, and one left open would hold
// the next caller until its own time ran out.
func TestGivingUpOnAGoneProcessOpensNoBackgroundTurn(t *testing.T) {
	sess := openOne(t, writeScript(t, "while IFS= read -r line; do "+resultLine+"done\n"), Policy{TurnTimeout: 5 * time.Second})
	sess.Close()
	select {
	case <-sess.exited:
	case <-time.After(5 * time.Second):
		t.Fatal("the process did not exit")
	}
	sess.giveUp(func(Turn) {})
	if sess.inBackground() {
		t.Error("a background turn was opened on a process that has gone")
	}
}

// A turn the session started by itself is not late: no caller asked for it.
func TestAnUnsolicitedTurnIsNotMarkedLate(t *testing.T) {
	sup, _, rec := newBGSupervisor(t, `while IFS= read -r line; do
  case "$line" in
    *later*) `+resultNow+`; ( sleep 0.2; `+assistantBG+`; `+resultBG+` ) & ;;
    *) `+resultOK+` ;;
  esac
done
`)
	send(t, sup, "later")
	if bg := rec.wait(t); bg.Late {
		t.Errorf("an unsolicited turn was marked late: %+v", bg)
	}
}
