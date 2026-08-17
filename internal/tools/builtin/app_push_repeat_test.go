package builtin

import "testing"

// A quarter of one day's notifications were the same two alerts repeated:
// "Google access expired" thirteen times, "Loop wa-monitor failed 3 times"
// twelve. Those describe conditions. "Sent to <someone>" and "Handled —
// <someone>" describe events that happen to share a title, and suppressing
// those would hide real activity rather than noise.
func TestOnlyConditionAlertsAreSuppressed(t *testing.T) {
	for _, kind := range []string{"alert", "loop", "update"} {
		if !repeatableKinds[kind] {
			t.Errorf("%q describes a condition and its repeats should be suppressed", kind)
		}
	}
	for _, kind := range []string{"reminder", "review", "briefing", "message"} {
		if repeatableKinds[kind] {
			t.Errorf("%q is an event; suppressing repeats would hide real activity", kind)
		}
	}
	if alertRepeatWindow < 1 {
		t.Error("the window must be positive or every alert is suppressed forever")
	}
}
