package builtin

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/MelloB1989/karmax/internal/bus"
	"github.com/MelloB1989/karmax/internal/store"
	"github.com/google/uuid"
)

// published captures the events a delegation announces.
func published(tool *ClaudeCodeTool) chan bus.Event {
	ch := make(chan bus.Event, 4)
	tool.Publish = func(e bus.Event) error { ch <- e; return nil }
	return ch
}

func waitEvent(t *testing.T, ch chan bus.Event) bus.Event {
	t.Helper()
	select {
	case e := <-ch:
		return e
	case <-time.After(20 * time.Second):
		t.Fatal("the delegation never announced its end")
	}
	return bus.Event{}
}

// A background delegation is written down before it starts — the session it
// runs under and whose request it serves — and its end is announced under an
// id derived from the job, carrying who is waiting.
func TestABackgroundDelegationIsJournalledAndAnnouncedWithItsOrigin(t *testing.T) {
	_, logPath := fakeClaudeHarness(t)
	tool, wd := newSessionKeyTestTool(t)
	events := published(tool)
	tool.Origin = func() DelegationOrigin {
		return DelegationOrigin{RequestID: "ev-1", ChannelID: "whatsapp-main", Target: "chat-7", Where: "the operator's own chat"}
	}

	res, err := tool.Execute(t.Context(), map[string]any{
		"prompt": "Build the Northwind onboarding deck", "background": true, "working_dir": wd,
	})
	if err != nil || res.IsError {
		t.Fatalf("execute: %v %v", err, res.Error)
	}
	jobID, _ := res.Output.(map[string]any)["job_id"].(string)

	evt := waitEvent(t, events)
	if evt.ID != DelegationEventID(jobID) {
		t.Errorf("announced as %q, want the job's own id", evt.ID)
	}
	if evt.Payload["origin_target"] != "chat-7" || evt.Payload["origin_request"] != "ev-1" {
		t.Errorf("the announcement lost who is waiting: %v", evt.Payload)
	}
	row, found, _ := tool.Store.GetTask(delegationRowID(jobID))
	if !found {
		t.Fatal("the delegation was never journalled")
	}
	if got := invocations(t, logPath); len(got) != 1 || got[0] != "new "+row.SessionKey {
		t.Errorf("CLI calls = %v, want one new session under the journalled id %s", got, row.SessionKey)
	}
	deadline := time.Now().Add(5 * time.Second)
	for row.Status != store.TaskDone && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
		row, _, _ = tool.Store.GetTask(delegationRowID(jobID))
	}
	if row.Status != store.TaskDone || row.RequestID != "ev-1" {
		t.Errorf("row = %+v, want it closed done, for its request", row)
	}
}

// crashedDelegation is the row a delegation leaves when the daemon dies mid-run.
func crashedDelegation(t *testing.T, tool *ClaudeCodeTool, wd string) (jobID, sid string) {
	t.Helper()
	jobID, sid = uuid.New().String(), uuid.New().String()
	raw, _ := json.Marshal(map[string]any{"prompt": "Build the deck", "working_dir": wd, freshSessionKey: sid})
	if _, err := tool.Store.CreateTask(store.Task{ID: delegationRowID(jobID), AgentID: "agent-1",
		Status: store.TaskDelegated, Goal: "Build the deck", SessionKey: sid, Progress: string(raw),
		RequestID: "ev-2", ChannelID: "whatsapp-main", Target: "chat-7"}); err != nil {
		t.Fatal(err)
	}
	return jobID, sid
}

// A delegation whose process died mid-way, after its session existed, goes
// on in that same session after a restart, and still reports to whoever was
// waiting.
func TestAnInterruptedDelegationResumesItsOwnSession(t *testing.T) {
	home, logPath := fakeClaudeHarness(t)
	tool, wd := newSessionKeyTestTool(t)
	events := published(tool)
	jobID, sid := crashedDelegation(t, tool, wd)
	dir := filepath.Join(home, ".claude", "projects", "work")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, sid+".jsonl"), []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if n := ResumeDelegations(*tool); n != 1 {
		t.Fatalf("resumed %d, want the one", n)
	}
	evt := waitEvent(t, events)
	if evt.ID != DelegationEventID(jobID) || evt.Payload["origin_target"] != "chat-7" {
		t.Errorf("announced %q with %v", evt.ID, evt.Payload)
	}
	if got := invocations(t, logPath); len(got) != 1 || got[0] != "resume "+sid {
		t.Errorf("CLI calls = %v, want the session resumed by its id", got)
	}
}

// One that died before its session was written starts that work again,
// under the same id.
func TestADelegationThatNeverStartedStartsAgainUnderItsId(t *testing.T) {
	_, logPath := fakeClaudeHarness(t)
	tool, wd := newSessionKeyTestTool(t)
	events := published(tool)
	_, sid := crashedDelegation(t, tool, wd)

	ResumeDelegations(*tool)
	waitEvent(t, events)
	if got := invocations(t, logPath); len(got) != 1 || got[0] != "new "+sid {
		t.Errorf("CLI calls = %v, want a new session under the journalled id", got)
	}
}

// One that had announced its end before the daemon died is not run again:
// its row is only closed.
func TestADelegationThatAlreadyAnnouncedIsNotRunAgain(t *testing.T) {
	_, logPath := fakeClaudeHarness(t)
	tool, wd := newSessionKeyTestTool(t)
	events := published(tool)
	jobID, _ := crashedDelegation(t, tool, wd)
	if _, err := tool.Store.AppendLogEvent(store.LogEvent{ID: DelegationEventID(jobID), Workspace: "test",
		Kind: string(bus.EventDelegationDone), CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}

	if n := ResumeDelegations(*tool); n != 0 {
		t.Errorf("resumed %d delegations that had already finished", n)
	}
	select {
	case e := <-events:
		t.Errorf("announced again: %+v", e)
	case <-time.After(200 * time.Millisecond):
	}
	if got := invocations(t, logPath); len(got) != 0 {
		t.Errorf("the CLI ran again: %v", got)
	}
	row, _, _ := tool.Store.GetTask(delegationRowID(jobID))
	if row.Status != store.TaskDone {
		t.Errorf("row status %q, want closed", row.Status)
	}
	if !strings.HasPrefix(interruptedPrompt("x"), "KARMAX restarted") {
		t.Error("a resumed session is not told why")
	}
}
