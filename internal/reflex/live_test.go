package reflex

import (
	"context"
	"os"
	"testing"

	"github.com/MelloB1989/karmax/internal/bus"
	"go.uber.org/zap"
)

// Against the real model, skipped without a key.
//
// The stubbed tests prove the wiring; this proves the sheet actually
// discriminates. Thresholds are calibrated from what it prints, so it logs the
// numbers rather than only asserting on them — a sheet that answers every
// message the same way is a sheet that costs money and decides nothing, and
// that failure is invisible to an assertion on any single case.
func TestLiveScreen(t *testing.T) {
	if os.Getenv("TYPESAFE_API_KEY") == "" {
		t.Skip("no TYPESAFE_API_KEY")
	}
	e, err := New(Config{Enabled: true}, zap.NewNop())
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	if e == nil {
		t.Fatal("a key is present but the evaluator is nil")
	}

	cases := []struct {
		name string
		body string
		want Action
	}{
		{"ack", "👍", ActionDrop},
		{"question", "Hey, can you send me the invoice for last month? Need it by tonight.", ActionHandle},
		{"fact", "FYI my new office address is 4th floor, Prestige Tech Park, Bangalore.", ActionRemember},
		{"heavy", "Can you fix the failing build on the payments service and open a PR?", ActionDelegate},
	}

	seen := map[Action]bool{}
	for _, tc := range cases {
		evt := bus.NewEvent(bus.EventCommsMessage, "agent", map[string]any{
			"content": tc.body, "channel_id": "919999999999@s.whatsapp.net",
		})
		v := e.Screen(context.Background(), evt, Hint{Sender: "A Contact"})
		t.Logf("%-9s -> %-9s effort=%-7s conf=%.2f urg=%.2f risk=%.2f remember=%v tokens=%d",
			tc.name, v.Action, v.Effort, v.Confidence, v.Urgency, v.Risk, v.Remember, v.InputTokens)

		if v.FailedOpen {
			t.Errorf("%s: failed open — %s", tc.name, v.Reason)
			continue
		}
		seen[v.Action] = true
		if v.Action != tc.want {
			// A warning, not a failure: these are calibration samples and the
			// model moves. A wrong answer here is a reason to look, not to
			// break the build.
			t.Logf("  note: wanted %s, got %s", tc.want, v.Action)
		}
	}
	if len(seen) < 2 {
		t.Errorf("every message got the same verdict (%v) — the sheet is not discriminating", seen)
	}
}
