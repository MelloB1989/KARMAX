package console

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/MelloB1989/karmax/fleet/internal/observe"
)

// eventsScript tails the hook log. fleetd's drain moves the file aside and
// the hook recreates it, so it must be -F (follow the name). -s makes tail poll:
// its inotify mode was observed to miss a rename-and-recreate in containers.
const eventsScript = `exec tail -n0 -F -s 0.2 "$HOME/.claude/fleet/events.jsonl"`

const (
	activityBucket = 15 * time.Minute
	activityWindow = 24 * time.Hour
	liveEventsCap  = 200
)

// IsStreamKind reports whether an event kind is one a person reads.
func IsStreamKind(kind string) bool {
	for _, k := range StreamKinds {
		if k == kind || strings.HasSuffix(k, ".") && strings.HasPrefix(kind, k) {
			return true
		}
	}
	return false
}

type liveAgent struct {
	tool *Tool
	last time.Time
}

// eventsSrc is the live tail of every live agent's hook log.
type eventsSrc struct {
	mu       sync.Mutex
	trs      map[string]*tracker
	agents   map[string]*liveAgent
	activity map[string]map[int64]int // agent → bucket start (unix) → events
	live     []Event                  // stream kinds, oldest first, id 0
	names    []string
}

func newEventsSrc(live []string) *eventsSrc {
	e := &eventsSrc{trs: map[string]*tracker{}, agents: map[string]*liveAgent{},
		activity: map[string]map[int64]int{}, names: live}
	for _, n := range live {
		e.trs[n] = newTracker(0, true)
		e.agents[n] = &liveAgent{}
		e.activity[n] = map[int64]int{}
	}
	return e
}

// handle takes one line of agent's hook log.
func (e *eventsSrc) handle(agent string, line []byte, now time.Time) bool {
	evs, _ := observe.ParseEvents(strings.NewReader(string(line)))
	if len(evs) != 1 {
		return false
	}
	ev := evs[0]
	at := ev.Time()
	e.mu.Lock()
	defer e.mu.Unlock()
	a := e.agents[agent]
	if at.After(a.last) {
		a.last = at
	}
	b := at.Truncate(activityBucket).Unix()
	e.activity[agent][b]++
	for k := range e.activity[agent] {
		if k < now.Add(-activityWindow).Unix() {
			delete(e.activity[agent], k)
		}
	}
	switch ev.Event {
	case "PreToolUse":
		a.tool = &Tool{Name: ev.Tool, Since: at}
	case "PostToolUse":
		if a.tool != nil && (ev.Tool == "" || ev.Tool == a.tool.Name) {
			a.tool = nil
		}
	case "Stop", "StopFailure", "SessionEnd":
		a.tool = nil
	}
	if IsStreamKind(ev.Event) {
		e.live = append(e.live, Event{At: at, Agent: agent, Kind: ev.Event, Session: ev.Session, Detail: ev.Raw})
		if len(e.live) > liveEventsCap {
			e.live = append([]Event(nil), e.live[len(e.live)-liveEventsCap:]...)
		}
	}
	return true
}

func (e *eventsSrc) run(ctx context.Context, sp spawner, prefix []string, container, agent string, now func() time.Time, changed func()) {
	argv := append(append([]string{}, prefix...), "exec", container, "sh", "-c", eventsScript)
	tr := e.trs[agent]
	streamLines(ctx, sp, argv, tr, now, func() { tr.success(now()); changed() }, func(l []byte) {
		if e.handle(agent, l, now()) {
			tr.success(now())
			changed()
		}
	})
}

func (e *eventsSrc) tool(agent string) (*Tool, time.Time) {
	e.mu.Lock()
	defer e.mu.Unlock()
	a := e.agents[agent]
	if a == nil {
		return nil, time.Time{}
	}
	var t *Tool
	if a.tool != nil {
		c := *a.tool
		t = &c
	}
	return t, a.last
}

func (e *eventsSrc) liveEvents() []Event {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]Event(nil), e.live...)
}

// buckets are agent's last 24 h of event counts, in 15-minute buckets, only
// those with events, oldest first.
func (e *eventsSrc) buckets(agent string, now time.Time) []Bucket {
	e.mu.Lock()
	defer e.mu.Unlock()
	var out []Bucket
	for k, n := range e.activity[agent] {
		if k >= now.Add(-activityWindow).Unix() {
			out = append(out, Bucket{T: time.Unix(k, 0).UTC(), N: n})
		}
	}
	sortBuckets(out)
	return out
}

func (e *eventsSrc) source(now time.Time) Source {
	return foldSources(e.names, func(n string) Source { return e.trs[n].source(now) })
}

func sortBuckets(b []Bucket) {
	for i := 1; i < len(b); i++ {
		for j := i; j > 0 && b[j].T.Before(b[j-1].T); j-- {
			b[j], b[j-1] = b[j-1], b[j]
		}
	}
}
