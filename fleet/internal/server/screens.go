package server

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/MelloB1989/karmax/fleet/internal/fleetapi"
	"github.com/MelloB1989/karmax/fleet/internal/term"
)

// Screen previews must not add load to the hosts: one exec per container
// captures every slot asked for, a capture is shared by every viewer for
// screenTTL, viewers who ask at once share the capture in flight, and at
// most capturesPerHost run on a host at once. A capture never starts a
// process in the agent's session: capture-pane reads tmux's own buffer.
const (
	screenTTL       = 2 * time.Second
	capturesPerHost = 2
	defaultLines    = 40
	maxLines        = 500
)

// screensRoutes registers GET /v1/screens?workers=a,b&lines=N: each worker's
// last N lines, ANSI kept, for those the caller may use. No workers is every
// worker the caller may use.
func screensRoutes(route func(string, func(http.ResponseWriter, *http.Request) (any, error), ...scope), ops Ops) {
	sc := newScreens()
	route("GET "+fleetapi.PathScreens, func(w http.ResponseWriter, r *http.Request) (any, error) {
		lines, err := intParam(r, "lines")
		if err != nil {
			return nil, err
		}
		if lines == 0 {
			lines = defaultLines
		}
		lines = min(lines, maxLines)
		asked := splitList(r.URL.Query().Get("workers"))
		if len(asked) == 0 {
			for _, row := range ops.Status(time.Now()) {
				if ops.IsWorker(row.Agent) {
					asked = append(asked, row.Agent)
				}
			}
		}
		out := fleetapi.ScreensReply{Screens: []fleetapi.Screen{}}
		var targets []term.Target
		var at []int // targets[i] answers out.Screens[at[i]]
		for _, wk := range asked {
			if !mayUseAgent(r, wk) {
				continue // not even whether it exists
			}
			tg, ok := ops.Terminal(wk)
			if !ok {
				out.Screens = append(out.Screens, fleetapi.Screen{Worker: wk, Error: "no such worker"})
				continue
			}
			at = append(at, len(out.Screens))
			out.Screens = append(out.Screens, fleetapi.Screen{Worker: wk})
			targets = append(targets, tg)
		}
		ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
		defer cancel()
		for i, s := range sc.get(ctx, targets, int(lines)) {
			out.Screens[at[i]] = s
		}
		return out, nil
	}, and(readers, member)...)
}

// screens is the shared capture cache.
type screens struct {
	now func() time.Time

	mu    sync.Mutex
	boxes map[string]*capture      // by container
	hosts map[string]chan struct{} // a host's capture slots
	busy  map[string]int           // captures running per host, for tests
}

// capture is a container's latest capture, or the one in flight.
type capture struct {
	at    time.Time
	lines int
	err   string
	panes map[string]shot // by tmux session
	// wait, while a capture runs, closes when it lands.
	wait chan struct{}
}

type shot struct {
	text string
	cols int
	err  string
}

func newScreens() *screens {
	return &screens{now: time.Now, boxes: map[string]*capture{}, hosts: map[string]chan struct{}{}, busy: map[string]int{}}
}

func boxKey(tg term.Target) string {
	return strings.Join(tg.Container.Exec, " ") + " " + tg.Container.Name
}

// get is each target's screen, in order, from captures at most screenTTL
// old.
func (s *screens) get(ctx context.Context, targets []term.Target, lines int) []fleetapi.Screen {
	byBox := map[string][]term.Target{}
	var order []string
	for _, tg := range targets {
		k := boxKey(tg)
		if _, ok := byBox[k]; !ok {
			order = append(order, k)
		}
		byBox[k] = append(byBox[k], tg)
	}
	caps := make(map[string]*capture, len(order))
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, k := range order {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c := s.box(ctx, k, byBox[k], lines)
			mu.Lock()
			caps[k] = c
			mu.Unlock()
		}()
	}
	wg.Wait()
	out := make([]fleetapi.Screen, len(targets))
	for i, tg := range targets {
		c := caps[boxKey(tg)]
		sc := fleetapi.Screen{Worker: tg.Worker, At: c.at}
		sh, ok := c.panes[tg.Pane]
		switch {
		case c.err != "":
			sc.Error = c.err
		case !ok || sh.err != "":
			sc.Error = sh.err
			if sc.Error == "" {
				sc.Error = "not captured"
			}
		default:
			sc.Text, sc.Cols = lastLines(sh.text, lines), sh.cols
		}
		out[i] = sc
	}
	return out
}

// box is a fresh enough capture of the container covering targets: the
// cached one, the one in flight, or a new one.
func (s *screens) box(ctx context.Context, key string, targets []term.Target, lines int) *capture {
	s.mu.Lock()
	for {
		c := s.boxes[key]
		if c == nil {
			break
		}
		if c.wait != nil {
			w := c.wait
			s.mu.Unlock()
			select {
			case <-w:
			case <-ctx.Done():
				return &capture{err: ctx.Err().Error()}
			}
			s.mu.Lock()
			continue
		}
		if s.now().Sub(c.at) < screenTTL && c.lines >= lines && covers(c, targets) {
			s.mu.Unlock()
			return c
		}
		break
	}
	// A new capture: of the panes asked for, and those the last one had, so
	// viewers of other slots of this container share it too.
	var panes []string
	add := func(p string) {
		if !slices.Contains(panes, p) {
			panes = append(panes, p)
		}
	}
	for _, tg := range targets {
		add(tg.Pane)
	}
	if old := s.boxes[key]; old != nil {
		for p := range old.panes {
			add(p)
		}
		lines = max(lines, old.lines)
	}
	slices.Sort(panes)
	c := &capture{lines: lines, wait: make(chan struct{})}
	s.boxes[key] = c
	host := targets[0].Host
	slot := s.hosts[host]
	if slot == nil {
		slot = make(chan struct{}, capturesPerHost)
		s.hosts[host] = slot
	}
	s.mu.Unlock()

	panesOut, err := s.capture(ctx, slot, host, targets[0], panes, lines)

	s.mu.Lock()
	c.at, c.panes = s.now(), panesOut
	if err != nil {
		c.err = err.Error()
	}
	close(c.wait)
	c.wait = nil
	s.mu.Unlock()
	return c
}

func covers(c *capture, targets []term.Target) bool {
	for _, tg := range targets {
		if _, ok := c.panes[tg.Pane]; !ok {
			return false
		}
	}
	return true
}

// captureScript prints each pane ($3…) after a marker line ($1) with its
// name and width; a pane that is not there has no width. $2 is the lines.
const captureScript = `m=$1; n=$2; shift 2
for p; do
	echo "$m $p $(tmux display-message -p -t "=$p:" '#{pane_width}' 2>/dev/null)"
	tmux capture-pane -p -e -t "=$p:" -S "-$n" 2>/dev/null
done
exit 0`

func (s *screens) capture(ctx context.Context, slot chan struct{}, host string, tg term.Target, panes []string, lines int) (map[string]shot, error) {
	select {
	case slot <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	s.mu.Lock()
	s.busy[host]++
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.busy[host]--
		s.mu.Unlock()
		<-slot
	}()
	marker := "==fleet-screen-" + newTermID() + "=="
	out, err := tg.Container.Sh(ctx, captureScript, append([]string{marker, strconv.Itoa(lines)}, panes...)...)
	if err != nil {
		// What docker said, without the command line: a guest sees this.
		why := err.Error()
		if i := strings.LastIndex(why, ": "); i >= 0 && strings.TrimSpace(why[i+2:]) != "" {
			why = why[i+2:]
		}
		return nil, fmt.Errorf("capture: %s", why)
	}
	got := map[string]shot{}
	var cur string
	var b strings.Builder
	flush := func() {
		if cur != "" {
			sh := got[cur]
			sh.text = b.String()
			got[cur] = sh
		}
		b.Reset()
	}
	for _, l := range strings.SplitAfter(string(out), "\n") {
		if rest, ok := strings.CutPrefix(l, marker+" "); ok {
			flush()
			f := strings.Fields(rest)
			if len(f) == 0 {
				cur = ""
				continue
			}
			cur = f[0]
			sh := shot{err: "no session " + cur}
			if len(f) > 1 {
				sh.cols, _ = strconv.Atoi(f[1])
				sh.err = ""
			}
			got[cur] = sh
			continue
		}
		b.WriteString(l)
	}
	flush()
	return got, nil
}

// lastLines is the last n lines of a capture, after its blank padding.
func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "" {
		lines = lines[:len(lines)-1]
	}
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// inflight and running are for tests: whether a capture of the container
// is in flight, and how many run on a host.
func (s *screens) inflight(name string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, c := range s.boxes {
		if strings.HasSuffix(k, " "+name) && c.wait != nil {
			return 1
		}
	}
	return 0
}

func (s *screens) running(host string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.busy[host]
}
