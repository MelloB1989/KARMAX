package harness

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// bgRecorder collects the background turns a supervisor reports.
type bgRecorder struct {
	mu    sync.Mutex
	turns []Turn
	keys  []string
	got   chan struct{}
}

func newBGRecorder() *bgRecorder { return &bgRecorder{got: make(chan struct{}, 16)} }

func (r *bgRecorder) record(key, kind string, t Turn) {
	r.mu.Lock()
	r.turns = append(r.turns, t)
	r.keys = append(r.keys, key+"/"+kind)
	r.mu.Unlock()
	r.got <- struct{}{}
}

func (r *bgRecorder) wait(t *testing.T) Turn {
	t.Helper()
	select {
	case <-r.got:
	case <-time.After(5 * time.Second):
		t.Fatal("no background turn was reported")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.turns[len(r.turns)-1]
}

// A fake CLI that answers every line, and after a line containing "later"
// starts a turn of its own — the shape of an inbound peer message arriving
// while the session is idle.
//
// The unsolicited turn is printed by a subshell so it can arrive while the
// session is between turns, or (with a slow result) while a caller's turn is
// waiting to start.
const assistantBG = `echo '{"type":"assistant","message":{"model":"m","content":[{"type":"text","text":"bg"},{"type":"tool_use","id":"t1","name":"SendMessage","input":{"to":"agent-03"}}]}}'`
const resultBG = `echo '{"type":"result","subtype":"success","is_error":false,"result":"bg","total_cost_usd":0.5,"usage":{"input_tokens":7,"output_tokens":3}}'`
const resultOK = `echo '{"type":"result","subtype":"success","is_error":false,"result":"ok","usage":{}}'`
const resultNow = `echo '{"type":"result","subtype":"success","is_error":false,"result":"ok-now","usage":{}}'`

func newBGSupervisor(t *testing.T, script string) (*Supervisor, *memStore, *bgRecorder) {
	t.Helper()
	st := newMemStore()
	rec := newBGRecorder()
	sup := New(Config{
		Binary: writeScript(t, script), WorkdirRoot: t.TempDir(), MaxLive: 4, Env: os.Environ(),
		Policies:         map[string]Policy{"agent": {TurnTimeout: 5 * time.Second, Idle: time.Minute}},
		OnBackgroundTurn: rec.record,
	}, st, NewBreaker(0.95, nil), testLog{t}, nil)
	t.Cleanup(sup.Shutdown)
	return sup, st, rec
}

func send(t *testing.T, sup *Supervisor, text string) string {
	t.Helper()
	turn, err := sup.Send(context.Background(), "agent:k", "agent", text)
	if err != nil {
		t.Fatalf("send %q: %v", text, err)
	}
	return turn.Text
}

// An unsolicited turn is handed to the background handler, and the next
// caller gets its own answer — not the background turn's result.
func TestAnUnsolicitedTurnGoesToTheBackgroundHandler(t *testing.T) {
	sup, st, rec := newBGSupervisor(t, `while IFS= read -r line; do
  case "$line" in
    *later*) `+resultNow+`; ( sleep 0.2; `+assistantBG+`; `+resultBG+` ) & ;;
    *) `+resultOK+` ;;
  esac
done
`)
	if got := send(t, sup, "later"); got != "ok-now" {
		t.Fatalf("first reply = %q", got)
	}
	bg := rec.wait(t)
	if bg.Text != "bg" || len(bg.ToolCalls) != 1 || bg.ToolCalls[0].Name != "SendMessage" {
		t.Errorf("background turn = %+v", bg)
	}
	if rec.keys[0] != "agent:k/agent" {
		t.Errorf("reported for %q", rec.keys[0])
	}
	if got := send(t, sup, "next"); got != "ok" {
		t.Fatalf("the next caller got %q: a background turn's result was misdelivered", got)
	}
	// The background turn is accounted like any other.
	if r, _ := st.GetHarnessSession("agent:k"); r.Turns != 3 || r.InputTokens != 7 {
		t.Errorf("recorded turns=%d input=%d, want 3 turns including the background one, 7 input tokens", r.Turns, r.InputTokens)
	}
}

// A caller arriving while a background turn is still running waits for it to
// finish before writing, so neither turn's events land on the other.
func TestSendWaitsForABackgroundTurnInProgress(t *testing.T) {
	sup, _, rec := newBGSupervisor(t, `while IFS= read -r line; do
  case "$line" in
    *later*) `+resultNow+`; ( sleep 0.1; `+assistantBG+`; sleep 0.8; `+resultBG+` ) & ;;
    *) `+resultOK+` ;;
  esac
done
`)
	send(t, sup, "later")
	time.Sleep(400 * time.Millisecond) // the background turn has begun, not ended
	if !sup.Busy("agent:k") {
		t.Error("a session mid background turn must look busy, or the reaper could close it")
	}
	if got := send(t, sup, "mine"); got != "ok" {
		t.Fatalf("caller got %q, want its own answer", got)
	}
	if bg := rec.wait(t); bg.Text != "bg" {
		t.Errorf("background turn text = %q", bg.Text)
	}
}

// A long unsolicited turn never stalls the process: today's 64-event buffer
// would fill with nobody reading it.
func TestALongBackgroundTurnDoesNotStallTheSession(t *testing.T) {
	sup, _, rec := newBGSupervisor(t, `while IFS= read -r line; do
  case "$line" in
    *later*) `+resultNow+`; ( for i in $(seq 1 300); do `+assistantBG+`; done; `+resultBG+` ) & ;;
    *) `+resultOK+` ;;
  esac
done
`)
	send(t, sup, "later")
	if bg := rec.wait(t); len(bg.ToolCalls) != 300 {
		t.Errorf("background turn carried %d tool calls, want 300", len(bg.ToolCalls))
	}
	if got := send(t, sup, "next"); got != "ok" {
		t.Fatalf("next reply = %q", got)
	}
}

// Housekeeping lines between turns (system notices, rate-limit updates, a
// stray result) are not a turn: the next caller must not wait on them.
func TestStrayLinesBetweenTurnsDoNotBlockTheNextCaller(t *testing.T) {
	sup, _, rec := newBGSupervisor(t, `while IFS= read -r line; do
  `+resultOK+`
  echo '{"type":"system","subtype":"status"}'
  echo '{"type":"rate_limit_event","rate_limit_info":{"status":"allowed"}}'
  echo '{"type":"result","subtype":"success","result":"stray"}'
done
`)
	send(t, sup, "one")
	time.Sleep(200 * time.Millisecond) // let the stray lines arrive while idle
	start := time.Now()
	if got := send(t, sup, "two"); got != "ok" {
		t.Fatalf("second reply = %q", got)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("second turn took %s: it waited on lines that were not a turn", d)
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.turns) != 0 {
		t.Errorf("housekeeping lines were reported as %d background turn(s)", len(rec.turns))
	}
}

// A session with no background handler configured still never misdelivers:
// background turns are dropped after being accounted.
func TestBackgroundTurnsWithoutAHandlerAreStillKeptOffTheNextCaller(t *testing.T) {
	st := newMemStore()
	sup := New(Config{
		Binary: writeScript(t, `while IFS= read -r line; do
  case "$line" in
    *later*) `+resultNow+`; ( sleep 0.1; `+assistantBG+`; `+resultBG+` ) & ;;
    *) `+resultOK+` ;;
  esac
done
`), WorkdirRoot: t.TempDir(), MaxLive: 4, Env: os.Environ(),
		Policies: map[string]Policy{"agent": {TurnTimeout: 5 * time.Second}},
	}, st, NewBreaker(0.95, nil), testLog{t}, nil)
	t.Cleanup(sup.Shutdown)
	send(t, sup, "later")
	time.Sleep(500 * time.Millisecond)
	if got := send(t, sup, "next"); strings.TrimSpace(got) != "ok" {
		t.Fatalf("next reply = %q", got)
	}
}

// A caller that gives up waiting behind a background turn has not seen the
// session fail: the turn another session asked for is still running and must
// not be killed for it.
func TestGivingUpOnABackgroundTurnLeavesTheSessionAlone(t *testing.T) {
	st := newMemStore()
	rec := newBGRecorder()
	sup := New(Config{
		Binary: writeScript(t, `while IFS= read -r line; do
  case "$line" in
    *later*) `+resultNow+`; ( sleep 0.1; `+assistantBG+`; sleep 2; `+resultBG+` ) & ;;
    *) `+resultOK+` ;;
  esac
done
`), WorkdirRoot: t.TempDir(), MaxLive: 4, Env: os.Environ(),
		Policies:         map[string]Policy{"agent": {TurnTimeout: 500 * time.Millisecond, Idle: time.Minute}},
		OnBackgroundTurn: rec.record,
	}, st, NewBreaker(0.95, nil), testLog{t}, nil)
	t.Cleanup(sup.Shutdown)
	send(t, sup, "later")
	time.Sleep(300 * time.Millisecond)

	_, err := sup.Send(context.Background(), "agent:k", "agent", "mine")
	if !errors.Is(err, ErrSessionBusy) {
		t.Fatalf("err = %v, want ErrSessionBusy", err)
	}
	if r, _ := st.GetHarnessSession("agent:k"); r.State == HarnessDead {
		t.Fatal("the session was killed for being busy")
	}
	if bg := rec.wait(t); bg.Text != "bg" {
		t.Fatalf("the background turn did not finish: %+v", bg)
	}
	if got := send(t, sup, "again"); got != "ok" {
		t.Fatalf("next reply = %q", got)
	}
}
