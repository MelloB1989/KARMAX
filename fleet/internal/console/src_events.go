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
// It runs under `docker exec -i` and ends when its stdin closes: killing the
// host-side docker exec does not kill tail inside the container, but it does
// close the stream (see leak.go). $1 is the slot's directory.
const eventsScript = `tail -n0 -F -s 0.2 "$HOME/.claude/fleet${1:+/$1}/events.jsonl" &
t=$!
trap 'kill $t 2>/dev/null' EXIT HUP TERM
cat >/dev/null`

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

// liveAgent is what one agent's hook log says. The log is shared by every
// claude process in the container — each `fleetctl tell` delivery runs a
// short `claude -p` whose hooks land in the same file — so tools are kept
// per session, and the snapshot shows only the agent's current one.
type liveAgent struct {
	tools map[string]*Tool // session → the tool it is running
	last  time.Time
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
		e.agents[n] = &liveAgent{tools: map[string]*Tool{}}
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
		a.tools[ev.Session] = &Tool{Name: ev.Tool, Since: at}
	case "PostToolUse":
		if t := a.tools[ev.Session]; t != nil && (ev.Tool == "" || ev.Tool == t.Name) {
			delete(a.tools, ev.Session)
		}
	case "Stop", "StopFailure", "SessionEnd":
		delete(a.tools, ev.Session)
	}
	if IsStreamKind(ev.Event) {
		e.live = append(e.live, Event{At: at, Agent: agent, Kind: ev.Event, Session: ev.Session, Detail: ev.Raw})
		if len(e.live) > liveEventsCap {
			e.live = append([]Event(nil), e.live[len(e.live)-liveEventsCap:]...)
		}
	}
	return true
}

func (e *eventsSrc) run(ctx context.Context, sp spawner, prefix []string, container, agent, slotDir string, now func() time.Time, changed func()) {
	argv := append(append([]string{}, prefix...), "exec", "-i", container)
	argv = append(argv, withStreamEnv("1", "sh", "-c", eventsScript, "sh", slotDir)...)
	tr := e.trs[agent]
	streamLines(ctx, sp, argv, tr, now, func() { tr.success(now()); changed() }, func(l []byte) {
		if e.handle(agent, l, now()) {
			tr.success(now())
			changed()
		}
	})
}

// tool is what the agent's current session is running. With no known
// session it is the most recent tool of any session.
func (e *eventsSrc) tool(agent, session string) (*Tool, time.Time) {
	e.mu.Lock()
	defer e.mu.Unlock()
	a := e.agents[agent]
	if a == nil {
		return nil, time.Time{}
	}
	var t *Tool
	if session != "" {
		t = a.tools[session]
	} else {
		for _, c := range a.tools {
			if t == nil || c.Since.After(t.Since) {
				t = c
			}
		}
	}
	if t != nil {
		c := *t
		t = &c
	}
	return t, a.last
}

// liveEvents are the stream-kind events seen live. current names an agent's
// current session ("" if unknown); an event from another session of that
// agent counts toward activity only and is left out.
func (e *eventsSrc) liveEvents(current func(agent string) string) []Event {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]Event, 0, len(e.live))
	for _, ev := range e.live {
		if current != nil {
			if cur := current(ev.Agent); cur != "" && ev.Session != cur {
				continue
			}
		}
		out = append(out, ev)
	}
	return out
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
