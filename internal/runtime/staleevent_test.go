package runtime

import (
	"testing"
	"time"

	"github.com/MelloB1989/karmax/internal/bus"
)

func TestStaleEventDropsWeeksOldConversation(t *testing.T) {
	evt := bus.Event{Kind: bus.EventCommsMessage, Timestamp: time.Now().Add(-7 * 24 * time.Hour)}
	if _, stale := staleEvent(evt); !stale {
		t.Error("a week-old message must not be answered as if it just arrived")
	}
}

func TestStaleEventKeepsLiveTraffic(t *testing.T) {
	for _, kind := range []bus.EventKind{bus.EventCommsMessage, bus.EventSystemCritical, bus.EventTimerFired} {
		evt := bus.Event{Kind: kind, Timestamp: time.Now().Add(-30 * time.Second)}
		if _, stale := staleEvent(evt); stale {
			t.Errorf("%s from 30s ago must still be delivered", kind)
		}
	}
}

func TestStaleEventLetsTimersFireLate(t *testing.T) {
	// A reminder armed for 09:00 while the daemon was down should still fire on
	// restart — that is the whole point of a durable timer.
	evt := bus.Event{Kind: bus.EventTimerFired, Timestamp: time.Now().Add(-3 * time.Hour)}
	if _, stale := staleEvent(evt); stale {
		t.Error("a timer three hours late must still fire")
	}
	old := bus.Event{Kind: bus.EventTimerFired, Timestamp: time.Now().Add(-30 * 24 * time.Hour)}
	if _, stale := staleEvent(old); !stale {
		t.Error("a month-old timer is a replay, not a reminder")
	}
}

func TestStaleEventKeepsUntimestampedEvents(t *testing.T) {
	// A zero timestamp is missing data, not evidence of age; dropping on it
	// would discard live work.
	if _, stale := staleEvent(bus.Event{Kind: bus.EventCommsMessage}); stale {
		t.Error("an event with no timestamp must not be treated as stale")
	}
}
