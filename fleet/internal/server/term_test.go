package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/MelloB1989/karmax/fleet/internal/config"
	"github.com/MelloB1989/karmax/fleet/internal/fleetapi"
	"github.com/MelloB1989/karmax/fleet/internal/target"
	"github.com/MelloB1989/karmax/fleet/internal/term"
)

// termFake is what the fake fleet's terminals are: a docker that logs every
// call and, for exec -it, runs a shell that prints its size and echoes; and
// a runner for the screens' captures.
var termFake struct {
	mu     sync.Mutex
	docker string // the fake docker's path; "" is docker
	runner target.Runner
	events []term.Event
}

// Terminal: the fake's agents live in fleet-<agent>, agent-02 on kali and
// the rest on pc2.
func (f *fake) Terminal(worker string) (term.Target, bool) {
	if !f.IsWorker(worker) {
		return term.Target{}, false
	}
	termFake.mu.Lock()
	defer termFake.mu.Unlock()
	agent, slot := config.SplitWorker(worker)
	host := "pc2"
	if agent == "agent-02" {
		host = "kali"
	}
	docker := termFake.docker
	if docker == "" {
		docker = "docker"
	}
	pane := "main"
	if slot > 1 {
		pane = fmt.Sprintf("main-%d", slot)
	}
	return term.Target{Worker: worker, Host: host, Pane: pane,
		Container: target.Container{Exec: []string{docker, "--context", host}, Name: "fleet-" + agent, Runner: termFake.runner}}, true
}

func (f *fake) RecordTerm(ev term.Event) {
	termFake.mu.Lock()
	defer termFake.mu.Unlock()
	termFake.events = append(termFake.events, ev)
}

// fakeDocker writes a docker that logs its argv, one call a line, to the
// returned log.
func fakeDocker(t *testing.T) (log string) {
	t.Helper()
	dir := t.TempDir()
	log = filepath.Join(dir, "calls")
	script := filepath.Join(dir, "docker")
	body := `#!/bin/sh
echo "$*" >> '` + log + `'
case " $* " in
*" -it "*) stty size; exec cat ;;
esac
exit 0
`
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	termFake.mu.Lock()
	termFake.docker, termFake.events = script, nil
	termFake.mu.Unlock()
	t.Cleanup(func() {
		termFake.mu.Lock()
		termFake.docker, termFake.runner, termFake.events = "", nil, nil
		termFake.mu.Unlock()
	})
	return log
}

// attachCall is the fake docker's exec -it call.
func attachCall(t *testing.T, log string) string {
	t.Helper()
	for _, c := range calls(t, log) {
		if strings.Contains(c, " -it ") {
			return c
		}
	}
	t.Fatalf("no exec -it among %q", calls(t, log))
	return ""
}

func calls(t *testing.T, log string) []string {
	t.Helper()
	b, _ := os.ReadFile(log)
	return strings.Split(strings.TrimSpace(string(b)), "\n")
}

// sock is a test's terminal client.
type sock struct {
	t  *testing.T
	c  *websocket.Conn
	mu sync.Mutex
	b  strings.Builder
	fr []fleetapi.TermFrame
	up chan struct{}
}

func dialTerm(t *testing.T, srv *httptest.Server, path, token string) (*sock, *http.Response, error) {
	t.Helper()
	h := http.Header{}
	if token != "" {
		h.Set("Authorization", "Bearer "+token)
	}
	ws, resp, err := websocket.Dial(context.Background(), "ws"+strings.TrimPrefix(srv.URL, "http")+path, &websocket.DialOptions{HTTPHeader: h})
	if err != nil {
		return nil, resp, err
	}
	ws.SetReadLimit(1 << 20)
	s := &sock{t: t, c: ws, up: make(chan struct{}, 1)}
	go func() {
		defer close(s.up)
		for {
			typ, b, err := ws.Read(context.Background())
			if err != nil {
				return
			}
			s.mu.Lock()
			if typ == websocket.MessageBinary {
				s.b.Write(b)
			} else {
				var f fleetapi.TermFrame
				json.Unmarshal(b, &f)
				s.fr = append(s.fr, f)
			}
			s.mu.Unlock()
			select {
			case s.up <- struct{}{}:
			default:
			}
		}
	}()
	t.Cleanup(func() { ws.CloseNow() })
	return s, resp, nil
}

func (s *sock) until(what string, ok func(out string, fr []fleetapi.TermFrame) bool) {
	s.t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		s.mu.Lock()
		done := ok(s.b.String(), slices.Clone(s.fr))
		out, fr := s.b.String(), slices.Clone(s.fr)
		s.mu.Unlock()
		if done {
			return
		}
		select {
		case _, open := <-s.up:
			if !open {
				s.mu.Lock()
				done := ok(s.b.String(), slices.Clone(s.fr))
				s.mu.Unlock()
				if !done {
					s.t.Fatalf("closed before %s; output %q, frames %+v", what, out, fr)
				}
				return
			}
		case <-deadline:
			s.t.Fatalf("no %s in 10s; output %q, frames %+v", what, out, fr)
		}
	}
}

func frameOf(typ string) func(string, []fleetapi.TermFrame) bool {
	return func(_ string, fr []fleetapi.TermFrame) bool {
		return slices.ContainsFunc(fr, func(f fleetapi.TermFrame) bool { return f.Type == typ })
	}
}

func (s *sock) frames() []fleetapi.TermFrame {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.fr)
}

func (s *sock) send(b []byte) {
	if err := s.c.Write(context.Background(), websocket.MessageBinary, b); err != nil {
		s.t.Fatal(err)
	}
}

func termServer(t *testing.T) *httptest.Server {
	t.Helper()
	_, h := setup(t)
	srv := httptest.NewServer(h)
	t.Cleanup(func() {
		// The sockets are closed by now (their cleanups ran first): wait for
		// every session to end, so none lands in the next test.
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			termFake.mu.Lock()
			open := 0
			for _, e := range termFake.events {
				if e.Phase == "start" {
					open++
				} else {
					open--
				}
			}
			termFake.mu.Unlock()
			if open <= 0 {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		srv.Close()
	})
	return srv
}

// waitEvents waits until n fleet.term events are recorded.
func waitEvents(t *testing.T, n int) []term.Event {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		termFake.mu.Lock()
		ev := slices.Clone(termFake.events)
		termFake.mu.Unlock()
		if len(ev) >= n || time.Now().After(deadline) {
			return ev
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestTermRefusesAWorkerTheCallerMayNotUse(t *testing.T) {
	log := fakeDocker(t)
	f, h, _ := tailnetSetup(t)
	// A guest lent another agent (agent-09) is refused before any upgrade.
	rec := tdo(t, h, "GET", "/v1/term/agent-03?mode=watch", friendIP, "", "")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("a guest's term: %d %s, want 403", rec.Code, rec.Body)
	}
	_, api := setup(t)
	for _, c := range []struct {
		path, token string
		want        int
	}{
		{"/v1/term/agent-03?mode=watch", "relay", 403},
		{"/v1/term/agent-03?mode=watch", "console", 403},
		{"/v1/term/agent-03?mode=watch", "", 401},
		{"/v1/term/agent-xyz?mode=watch", "full", 404},
		{"/v1/term/agent-03?mode=dance", "full", 400},
	} {
		if rec := do(t, api, "GET", c.path, c.token, ""); rec.Code != c.want {
			t.Errorf("%s as %q: %d %s, want %d", c.path, c.token, rec.Code, rec.Body, c.want)
		}
	}
	if b, _ := os.ReadFile(log); len(b) != 0 || len(f.calls) != 0 {
		t.Fatalf("a refused term reached the fleet: %q %v", b, f.calls)
	}
}

func TestTermBuildsTheRightCommandPerMode(t *testing.T) {
	for _, c := range []struct {
		path, want string
	}{
		{"/v1/term/agent-03:2?mode=watch&cols=100&rows=30", "--context pc2 exec -it -e FLEET_TERM=* fleet-agent-03 tmux attach -t =main-2 -f read-only,ignore-size"},
		{"/v1/term/agent-03:2?mode=type&cols=100&rows=30", "--context pc2 exec -it -e FLEET_TERM=* fleet-agent-03 tmux attach -t =main-2"},
		{"/v1/term/agent-02?mode=shell&cols=100&rows=30", "--context kali exec -it -e FLEET_TERM=* -w /work fleet-agent-02 bash -l"},
		{"/v1/term/agent-02?cols=100&rows=30", "--context kali exec -it -e FLEET_TERM=* fleet-agent-02 tmux attach -t =main -f read-only,ignore-size"},
	} {
		t.Run(c.path, func(t *testing.T) {
			log := fakeDocker(t)
			srv := termServer(t)
			s, _, err := dialTerm(t, srv, c.path, "full")
			if err != nil {
				t.Fatal(err)
			}
			s.until("the size", func(out string, _ []fleetapi.TermFrame) bool { return strings.Contains(out, "30 100") })
			got := attachCall(t, log)
			pre, post, _ := strings.Cut(c.want, "*")
			if !strings.HasPrefix(got, pre) || !strings.HasSuffix(got, post) {
				t.Fatalf("argv = %q, want %q", got, c.want)
			}
			if h := s.frames()[0]; h.Type != fleetapi.FrameHello || h.Cols != 100 || h.Rows != 30 {
				t.Fatalf("first frame = %+v, want hello with the size", h)
			}
		})
	}
}

func TestATypistSizesTheWindowAndLeavesItAsConfigured(t *testing.T) {
	log := fakeDocker(t)
	srv := termServer(t)
	s, _, err := dialTerm(t, srv, "/v1/term/agent-03:2?mode=type&cols=100&rows=30", "full")
	if err != nil {
		t.Fatal(err)
	}
	s.until("the size", func(out string, _ []fleetapi.TermFrame) bool { return strings.Contains(out, "30 100") })
	s.c.Close(websocket.StatusNormalClosure, "")
	ev := waitEvents(t, 2)
	if len(ev) != 2 || ev[0].Phase != "start" || ev[1].Phase != "end" || ev[1].Who != "operator" || ev[1].Mode != fleetapi.TermType {
		t.Fatalf("events = %+v, want a start and an end", ev)
	}
	got := strings.Join(calls(t, log), "\n")
	for _, want := range []string{
		"tmux resize-window -t =main-2: -x 100 -y 30",
		"tmux resize-window -t =main-2: -x 220 -y 50",
		"FLEET_TERM=", // the cleanup of what the killed exec left
	} {
		if !strings.Contains(got, want) {
			t.Errorf("calls lack %q:\n%s", want, got)
		}
	}
	if i, j := strings.Index(got, "-x 100 -y 30"), strings.Index(got, "-x 220 -y 50"); i > j {
		t.Errorf("the window was restored before the typist sized it:\n%s", got)
	}
}

func TestAWatcherNeverResizesTheWindow(t *testing.T) {
	log := fakeDocker(t)
	srv := termServer(t)
	s, _, err := dialTerm(t, srv, "/v1/term/agent-03?mode=watch&cols=100&rows=30", "full")
	if err != nil {
		t.Fatal(err)
	}
	s.until("the size", func(out string, _ []fleetapi.TermFrame) bool { return strings.Contains(out, "30 100") })
	b, _ := json.Marshal(fleetapi.TermFrame{Type: fleetapi.FrameResize, Cols: 60, Rows: 20})
	s.c.Write(context.Background(), websocket.MessageText, b)
	s.send([]byte("typed\n"))
	s.c.Close(websocket.StatusNormalClosure, "")
	waitEvents(t, 2)
	got := strings.Join(calls(t, log), "\n")
	if strings.Contains(got, "resize-window") {
		t.Fatalf("a watcher resized the window:\n%s", got)
	}
}

func TestASecondTypistIsHeldAndAnAdminMayForce(t *testing.T) {
	fakeDocker(t)
	srv := termServer(t)
	first, _, err := dialTerm(t, srv, "/v1/term/agent-03?mode=type&cols=80&rows=24", "full")
	if err != nil {
		t.Fatal(err)
	}
	first.until("hello", frameOf(fleetapi.FrameHello))

	second, _, err := dialTerm(t, srv, "/v1/term/agent-03?mode=type&cols=80&rows=24", "full")
	if err != nil {
		t.Fatal(err)
	}
	second.until("held", frameOf(fleetapi.FrameHeld))
	if h := second.frames()[0]; h.Holder != "operator" {
		t.Fatalf("held frame = %+v, want the holder", h)
	}

	// A watcher is never held.
	w, _, err := dialTerm(t, srv, "/v1/term/agent-03?mode=watch&cols=80&rows=24", "full")
	if err != nil {
		t.Fatal(err)
	}
	w.until("hello", frameOf(fleetapi.FrameHello))

	forced, _, err := dialTerm(t, srv, "/v1/term/agent-03?mode=type&force=1&cols=80&rows=24", "full")
	if err != nil {
		t.Fatal(err)
	}
	forced.until("hello", frameOf(fleetapi.FrameHello))
	first.until("released", frameOf(fleetapi.FrameReleased))
	// The holder watches now: a second hello, as a watcher.
	first.until("a watcher's hello", func(_ string, fr []fleetapi.TermFrame) bool {
		return slices.ContainsFunc(fr, func(f fleetapi.TermFrame) bool { return f.Type == fleetapi.FrameHello && f.Mode == fleetapi.TermWatch })
	})
}

func TestOnlyAnAdminMayForce(t *testing.T) {
	_, h, _ := tailnetSetup(t)
	// A guest is refused the worker before force is weighed; force itself is
	// refused for anyone who is not an admin, before the upgrade.
	if rec := tdo(t, h, "GET", "/v1/term/agent-03?mode=type&force=1", ramIP, "", ""); rec.Code != http.StatusForbidden {
		t.Fatalf("a guest's force: %d, want 403", rec.Code)
	}
}

func TestTermEndsWhenTheContainerIsGone(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "docker")
	os.WriteFile(script, []byte("#!/bin/sh\necho 'Error response from daemon: container fleet-agent-03 is not running'\nexit 1\n"), 0o755)
	termFake.mu.Lock()
	termFake.docker = script
	termFake.mu.Unlock()
	t.Cleanup(func() { termFake.mu.Lock(); termFake.docker = ""; termFake.mu.Unlock() })
	srv := termServer(t)
	s, _, err := dialTerm(t, srv, "/v1/term/agent-03?mode=watch", "full")
	if err != nil {
		t.Fatal(err)
	}
	s.until("an error frame", frameOf(fleetapi.FrameError))
	fr := s.frames()
	if e := fr[len(fr)-1]; !strings.Contains(e.Error, "is not running") {
		t.Fatalf("error frame = %+v", e)
	}
}

// ---- screens ----

// captures is a runner that answers the screens' capture script, counting
// its calls per container.
type captures struct {
	mu    sync.Mutex
	calls map[string]int
	gate  chan struct{} // when set, each call waits for it
	fail  bool
}

func (c *captures) Run(ctx context.Context, argv []string, _ io.Reader) ([]byte, error) {
	if c.gate != nil {
		<-c.gate
	}
	// … exec fleet-agent-03 sh -c <script> sh <marker> <lines> <pane>…
	i := slices.Index(argv, "exec")
	name := argv[i+1]
	c.mu.Lock()
	c.calls[name]++
	n := c.calls[name]
	c.mu.Unlock()
	if c.fail {
		return nil, errors.New("Error response from daemon: container " + name + " is not running")
	}
	args := argv[i+6:]
	marker, panes := args[0], args[2:]
	var b strings.Builder
	for _, p := range panes {
		fmt.Fprintf(&b, "%s %s 220\n", marker, p)
		fmt.Fprintf(&b, "line one of %s\nscreen %d of %s %s\n\n\n", name, n, name, p)
	}
	return []byte(b.String()), nil
}

func (c *captures) count() map[string]int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return maps.Clone(c.calls)
}

func useCaptures(t *testing.T) *captures {
	c := &captures{calls: map[string]int{}}
	termFake.mu.Lock()
	termFake.runner = c
	termFake.mu.Unlock()
	t.Cleanup(func() { termFake.mu.Lock(); termFake.runner = nil; termFake.mu.Unlock() })
	return c
}

func screensOf(t *testing.T, h http.Handler, path, token string) fleetapi.ScreensReply {
	t.Helper()
	rec := do(t, h, "GET", path, token, "")
	if rec.Code != 200 {
		t.Fatalf("%s: %d %s", path, rec.Code, rec.Body)
	}
	var out fleetapi.ScreensReply
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestScreensTakeOneExecPerContainer(t *testing.T) {
	c := useCaptures(t)
	_, h := setup(t)
	got := screensOf(t, h, "/v1/screens?workers=agent-03,agent-03:2,agent-02,agent-02:2&lines=1", "full")
	if n := c.count(); n["fleet-agent-03"] != 1 || n["fleet-agent-02"] != 1 || len(n) != 2 {
		t.Fatalf("execs = %v, want one per container", n)
	}
	var names []string
	for _, s := range got.Screens {
		names = append(names, s.Worker)
		if s.Cols != 220 || s.Error != "" || s.At.IsZero() {
			t.Errorf("screen %+v", s)
		}
	}
	if !slices.Equal(names, []string{"agent-03", "agent-03:2", "agent-02", "agent-02:2"}) {
		t.Fatalf("screens = %v, want the asked order", names)
	}
	// lines=1 is the last line with text: blank padding is not a line.
	if s := got.Screens[1]; s.Text != "screen 1 of fleet-agent-03 main-2" {
		t.Fatalf("agent-03:2's text = %q", s.Text)
	}
}

func TestScreensAreSharedForTwoSeconds(t *testing.T) {
	c := useCaptures(t)
	now := time.Date(2026, 10, 25, 21, 0, 0, 0, time.UTC)
	sc := newScreens()
	sc.now = func() time.Time { return now }
	ops := &fake{}
	get := func(workers ...string) []fleetapi.Screen {
		var ts []term.Target
		for _, w := range workers {
			tg, _ := ops.Terminal(w)
			ts = append(ts, tg)
		}
		return sc.get(context.Background(), ts, 40)
	}
	get("agent-03")
	get("agent-03")   // a second viewer, at once: the same capture
	get("agent-03:2") // a slot not captured yet: one exec, for both
	now = now.Add(1900 * time.Millisecond)
	get("agent-03", "agent-03:2")
	if n := c.count()["fleet-agent-03"]; n != 2 {
		t.Fatalf("within two seconds: %d execs, want 2 (the first, and the one that added a slot)", n)
	}
	now = now.Add(2 * time.Second)
	s := get("agent-03")
	if n := c.count()["fleet-agent-03"]; n != 3 {
		t.Fatalf("after two seconds: %d execs, want a fresh one", n)
	}
	if !strings.Contains(s[0].Text, "screen 3") {
		t.Fatalf("text = %q, want the fresh capture", s[0].Text)
	}

	// Viewers who ask at the same moment share one capture in flight.
	c.gate = make(chan struct{})
	now = now.Add(time.Minute)
	var wg sync.WaitGroup
	for range 5 {
		wg.Add(1)
		go func() { defer wg.Done(); get("agent-03") }()
	}
	for sc.inflight("fleet-agent-03") == 0 {
		time.Sleep(time.Millisecond)
	}
	close(c.gate)
	wg.Wait()
	if n := c.count()["fleet-agent-03"]; n != 4 {
		t.Fatalf("five viewers at once: %d execs, want one more", n-3)
	}
}

func TestScreensRunAtMostTwoCapturesPerHost(t *testing.T) {
	c := useCaptures(t)
	c.gate = make(chan struct{})
	sc := newScreens()
	ops := &fake{}
	var wg sync.WaitGroup
	for _, a := range []string{"agent-03", "agent-04", "agent-05", "agent-06"} { // four containers on pc2
		wg.Add(1)
		go func() {
			defer wg.Done()
			tg, _ := ops.Terminal(a)
			sc.get(context.Background(), []term.Target{tg}, 10)
		}()
	}
	for sc.running("pc2") < 2 {
		time.Sleep(time.Millisecond)
	}
	time.Sleep(20 * time.Millisecond) // a third, were it let through, would be counted by now
	if n := sc.running("pc2"); n != 2 {
		t.Fatalf("%d captures at once on pc2, want 2", n)
	}
	close(c.gate)
	wg.Wait()
}

func TestScreensFilterWhatTheCallerMayUse(t *testing.T) {
	useCaptures(t)
	_, h, _ := tailnetSetup(t)
	rec := tdo(t, h, "GET", "/v1/screens?workers=agent-03,agent-02", friendIP, "", "") // lent agent-09 only
	if rec.Code != 200 {
		t.Fatalf("a guest's screens: %d %s", rec.Code, rec.Body)
	}
	var got fleetapi.ScreensReply
	json.Unmarshal(rec.Body.Bytes(), &got)
	if len(got.Screens) != 0 {
		t.Fatalf("a guest lent neither saw %+v", got.Screens)
	}
	_, api := setup(t)
	for _, tok := range []string{"relay", ""} {
		if rec := do(t, api, "GET", "/v1/screens?workers=agent-03", tok, ""); rec.Code != 403 && rec.Code != 401 {
			t.Errorf("screens as %q: %d", tok, rec.Code)
		}
	}
	// The console reads them; an unknown worker says so.
	s := screensOf(t, api, "/v1/screens?workers=agent-03,agent-xyz", "console")
	if len(s.Screens) != 2 || s.Screens[1].Error == "" {
		t.Fatalf("screens = %+v", s.Screens)
	}
}

func TestScreensWithoutWorkersAreEveryoneTheCallerMaySee(t *testing.T) {
	c := useCaptures(t)
	_, h := setup(t)
	got := screensOf(t, h, "/v1/screens", "full")
	if len(got.Screens) != 2 { // the fake's status rows: agent-03 and agent-02
		t.Fatalf("screens = %+v", got.Screens)
	}
	c.fail = true
	s := newScreens()
	tg, _ := (&fake{}).Terminal("agent-05")
	if r := s.get(context.Background(), []term.Target{tg}, 10); r[0].Error == "" || !strings.Contains(r[0].Error, "not running") {
		t.Fatalf("a failed capture: %+v, want its error", r[0])
	}
}
