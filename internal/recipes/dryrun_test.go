package recipes

import (
	"context"
	"strings"
	"testing"

	"github.com/MelloB1989/karmax/pkg/loopkit"
)

func TestDryRunHarnessWithRecordsTheSpecAndEchoesTheSessionID(t *testing.T) {
	d := NewDryRun(loopkit.Trigger{Kind: loopkit.TriggerManual})
	res, err := d.HarnessWith(context.Background(), loopkit.HarnessSpec{
		Prompt: "do the thing", SessionID: "lyzn:t1", WorkingDir: "lyzn-tasks/t1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.SessionID != "lyzn:t1" {
		t.Fatalf("session id = %q, want it echoed back", res.SessionID)
	}
	if !strings.Contains(d.Report(), "lyzn:t1") {
		t.Fatalf("report does not mention the session: %s", d.Report())
	}
}

func TestDryRunHarnessForgetRecordsWithoutErroring(t *testing.T) {
	d := NewDryRun(loopkit.Trigger{Kind: loopkit.TriggerManual})
	if err := d.HarnessForget("lyzn:t1", "lyzn-tasks/t1"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(d.Report(), "lyzn:t1") {
		t.Fatalf("report does not mention the forgotten session: %s", d.Report())
	}
}
