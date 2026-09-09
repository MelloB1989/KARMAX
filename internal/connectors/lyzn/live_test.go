package lyzn

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/MelloB1989/karmax/pkg/connectorkit"
)

// The same sequence as the stubbed round trip, against a LYZN that is really
// there. Skipped unless it is told where.
//
//	LYZN_API=https://api.lyzn.ai \
//	LYZN_PAIR_CODE=K7QD2M \
//	go test ./internal/connectors/lyzn/ -run Live -v
//
// It **spends the code**, because that is the thing being tested: a code is
// worth one machine, once. Everything after the pairing is read-only —
// heartbeat and the work queue — unless `LYZN_LIVE_WORK=1` is set, which
// makes it claim the first approved task and report it done. That prints a
// receipt on somebody's phone for work nobody did, so it is deliberately not
// the default and must not be pointed at an account in real use.
//
// The machine this leaves behind is real and stays paired. Unpair it in the
// app: Settings → Laptop daemon.
func TestLiveRoundTrip(t *testing.T) {
	api := strings.TrimSpace(os.Getenv("LYZN_API"))
	code := strings.TrimSpace(os.Getenv("LYZN_PAIR_CODE"))
	if api == "" || code == "" {
		t.Skip("set LYZN_API and LYZN_PAIR_CODE to run this against a real LYZN")
	}

	ctx := context.Background()
	c := New()

	if err := c.ValidateCredentials(connectorkit.Credentials{
		Config: map[string]string{keyCode: code},
	}); err != nil {
		t.Fatalf("the code was refused before it was sent: %v", err)
	}

	filled, err := c.CompleteCredentials(ctx, connectorkit.Credentials{
		Config: map[string]string{keyAPI: api, keyCode: code, keyName: "karmax-live-test"},
	})
	if err != nil {
		t.Fatalf("pairing: %v", err)
	}
	if filled[keyToken] == "" {
		t.Fatal("paired, but no token came back")
	}
	t.Logf("paired as daemon %s, machine %q", filled[keyDaemonID], filled[keyName])

	paired := connectorkit.Credentials{Config: map[string]string{
		keyAPI: api, keyToken: filled[keyToken], keyDaemonID: filled[keyDaemonID],
	}}

	if err := c.Health(ctx, paired); err != nil {
		t.Fatalf("health, which is also the heartbeat: %v", err)
	}

	state, err := beat(ctx, paired)
	if err != nil {
		t.Fatalf("heartbeat: %v", err)
	}
	t.Logf("the account has %d approved task(s) waiting", state.Tasks)

	events, cursor, err := pollWork(ctx, paired, "")
	if err != nil {
		t.Fatalf("poll: %v", err)
	}
	t.Logf("the poll announced %d task(s); cursor %s", len(events), cursor)
	if len(events) != state.Tasks {
		t.Fatalf("the beat said %d and the queue held %d — they are the same query and must agree",
			state.Tasks, len(events))
	}
	for _, e := range events {
		t.Logf("  %s  %q  (from %q)", e["task_id"], e["text"], e["title"])
	}

	// A second poll with the cursor the first returned says nothing, which is
	// the property that keeps a minute-by-minute loop from re-announcing the
	// same promise forever.
	again, _, err := pollWork(ctx, paired, cursor)
	if err != nil {
		t.Fatalf("second poll: %v", err)
	}
	if len(again) != 0 {
		t.Fatalf("the same tasks were announced twice: %v", again)
	}

	if os.Getenv("LYZN_LIVE_WORK") != "1" || len(events) == 0 {
		t.Log("stopping before the work; set LYZN_LIVE_WORK=1 to claim and report one")
		return
	}

	id, _ := events[0]["task_id"].(string)
	claimed, err := claimWork(ctx, paired, map[string]any{"task_id": id})
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	t.Logf("claimed %s: %v", id, claimed.(map[string]any)["task"].(map[string]any)["text"])

	out, err := reportWork(ctx, paired, map[string]any{
		"task_id": id, "outcome": "done",
		"summary": "Carried out by KARMAX's live connector test.",
		"artifacts": []any{
			map[string]any{"name": "live-test.log", "uri": "file:///tmp/lyzn-live-test.log"},
		},
	})
	if err != nil {
		t.Fatalf("report: %v", err)
	}
	receipt, ok := out.(map[string]any)["receipt"].(map[string]any)
	if !ok || receipt["receiptId"] == nil {
		t.Fatalf("the task closed without a receipt: %v", out)
	}
	t.Logf("receipt %v printed", receipt["receiptId"])

	// Idempotent: the same result again is the same receipt.
	repeat, err := reportWork(ctx, paired, map[string]any{
		"task_id": id, "outcome": "done", "summary": "Carried out by KARMAX's live connector test.",
	})
	if err != nil {
		t.Fatalf("repeat report: %v", err)
	}
	if repeat.(map[string]any)["receipt"].(map[string]any)["receiptId"] != receipt["receiptId"] {
		t.Fatal("a second receipt was printed for one promise")
	}

	// And the task has left the queue.
	after, _, err := pollWork(ctx, paired, "")
	if err != nil {
		t.Fatalf("poll after: %v", err)
	}
	for _, e := range after {
		if e["task_id"] == id {
			t.Fatal("a finished task is still being handed out")
		}
	}
	t.Logf("%d task(s) left waiting", len(after))
}
