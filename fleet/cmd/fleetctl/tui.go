package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"golang.org/x/term"

	"github.com/MelloB1989/karmax/fleet/internal/daemon"
	"github.com/MelloB1989/karmax/fleet/internal/fleetapi"
	"github.com/MelloB1989/karmax/fleet/internal/identity"
	"github.com/MelloB1989/karmax/fleet/internal/server"
	"github.com/MelloB1989/karmax/fleet/internal/tui"
)

// tuiCmd is `fleetctl tui [--demo [--guest]] [--no-anim]`: the full-screen
// TUI on fleetd, or on a made-up fleet with --demo.
func tuiCmd(args []string) error {
	var demo, guest, noAnim bool
	_, pos, err := flags("tui", args, func(fs *flag.FlagSet) {
		fs.BoolVar(&demo, "demo", false, "")
		fs.BoolVar(&guest, "guest", false, "")
		fs.BoolVar(&noAnim, "no-anim", false, "")
	})
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return errors.New("usage: fleetctl tui [--demo [--guest]] [--no-anim]")
	}
	if guest && !demo {
		return errors.New("--guest shows the demo as a guest sees it: add --demo")
	}
	if !stdioIsTerminal() {
		return errors.New("fleetctl tui needs a terminal")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	opts := tui.Options{Version: server.Version, NoAnim: noAnim || os.Getenv("NO_COLOR") != "", Attach: AttachCommand}
	if demo {
		role := fleetapi.RoleAdmin
		if guest {
			role = fleetapi.RoleGuest
		}
		f := fleetapi.NewFake(role, time.Now)
		go stepEvery(ctx, f, 2*time.Second)
		opts.Client, opts.Attach = f, demoAttach // its agents are made up: no session to join
	} else {
		c, err := newClient()
		if err != nil {
			return err
		}
		opts.Client = &apiClient{c: c, from: sessionName()}
	}
	return tui.Run(ctx, opts)
}

// stepEvery moves the demo fleet on until ctx ends.
func stepEvery(ctx context.Context, f *fleetapi.Fake, d time.Duration) {
	t := time.NewTicker(d)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			f.Step()
		}
	}
}

// stdioIsTerminal: both stdin and stdout are a terminal, so plain
// `fleetctl` may open the TUI.
func stdioIsTerminal() bool {
	return term.IsTerminal(int(os.Stdin.Fd())) && term.IsTerminal(int(os.Stdout.Fd()))
}

// demoAttach is ssh in the demo, whose agents have no sessions.
func demoAttach(worker string, mode fleetapi.TermMode) tea.ExecCommand {
	return demoSSH{}
}

type demoSSH struct{}

func (demoSSH) Run() error {
	return errors.New("the demo's agents are made up: run fleetctl tui to ssh into real ones")
}
func (demoSSH) SetStdin(io.Reader)  {}
func (demoSSH) SetStdout(io.Writer) {}
func (demoSSH) SetStderr(io.Writer) {}

// apiClient is fleetapi.Client over fleetd's routes, the same ones the CLI
// commands use, as the session from.
type apiClient struct {
	c    *client
	from string
}

// watchTimeout outlasts fleetd's 25-second long poll.
const watchTimeout = 60 * time.Second

// do is client.do with a context: a call the TUI gives up on (it quit, a
// newer one superseded it) stops at once.
func (a *apiClient) do(ctx context.Context, method, path string, in, out any, timeout time.Duration) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, a.c.base+path, body)
	if err != nil {
		return err
	}
	if a.c.token != "" {
		req.Header.Set("Authorization", "Bearer "+a.c.token)
	}
	if a.c.agent != "" {
		req.Header.Set(identity.Header, a.c.agent)
	}
	req.Header.Set("Content-Type", "application/json")
	tr := &http.Transport{Proxy: http.ProxyFromEnvironment, DialContext: (&net.Dialer{Timeout: 10 * time.Second}).DialContext}
	resp, err := (&http.Client{Transport: tr}).Do(req)
	if err != nil {
		if ctx.Err() != nil && errors.Is(err, context.Canceled) {
			return err
		}
		// The call's own time ran out (not a dial that failed): fleetd is
		// there, and busy.
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return fmt.Errorf("fleetd at %s did not answer in time: %w", a.c.base, context.DeadlineExceeded)
		}
		return fmt.Errorf("fleetd unreachable at %s: %w", a.c.base, err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode >= 300 {
		var e struct{ Error string }
		if json.Unmarshal(b, &e) == nil && e.Error != "" {
			return &statusError{Code: resp.StatusCode, msg: e.Error, Body: b}
		}
		return &statusError{Code: resp.StatusCode, msg: "fleetd: " + resp.Status, Body: b}
	}
	if out != nil {
		return json.Unmarshal(b, out)
	}
	return nil
}

func (a *apiClient) Watch(ctx context.Context, since uint64) (*fleetapi.Snapshot, error) {
	var s fleetapi.Snapshot
	err := a.do(ctx, "GET", fleetapi.PathWatch+"?since="+fmt.Sprint(since), nil, &s, watchTimeout)
	var se *statusError
	if errors.As(err, &se) && se.Code == http.StatusNotFound {
		return nil, errors.New("this fleetd has no " + fleetapi.PathWatch + ": it is older than this fleetctl")
	}
	if err != nil {
		return nil, err
	}
	return &s, nil
}

func (a *apiClient) Screens(ctx context.Context, workers []string, lines int) ([]fleetapi.Screen, error) {
	q := url.Values{"workers": {strings.Join(workers, ",")}, "lines": {fmt.Sprint(lines)}}
	var r fleetapi.ScreensReply
	if err := a.do(ctx, "GET", fleetapi.PathScreens+"?"+q.Encode(), nil, &r, callTimeout); err != nil {
		return nil, err
	}
	return r.Screens, nil
}

// callTimeout bounds every call but the watch.
const callTimeout = 30 * time.Second

func taskPath(id, verb string) string {
	p := "/v1/tasks/" + url.PathEscape(id)
	if verb != "" {
		p += "/" + verb
	}
	return p
}

func (a *apiClient) Task(ctx context.Context, id string) (*fleetapi.TaskDetail, error) {
	var d fleetapi.TaskDetail
	if err := a.do(ctx, "GET", taskPath(id, ""), nil, &d, callTimeout); err != nil {
		return nil, err
	}
	return &d, nil
}

func (a *apiClient) Run(ctx context.Context, r fleetapi.RunRequest) (*fleetapi.RunReply, error) {
	req := map[string]any{"brief": r.Brief, "repo": r.Repo, "agent": r.Agent, "from": a.from, "task": daemon.TaskID(r.Brief, time.Now())}
	if r.Model != "" {
		req["model"] = r.Model
	}
	var rep fleetapi.RunReply
	if err := a.do(ctx, "POST", "/v1/run", req, &rep, callTimeout); err != nil {
		return nil, err
	}
	return &rep, nil
}

func (a *apiClient) Followup(ctx context.Context, task, text string) error {
	return a.do(ctx, "POST", taskPath(task, "followup"), map[string]string{"from": a.from, "text": text}, nil, callTimeout)
}

func (a *apiClient) Answer(ctx context.Context, task, text string) error {
	return a.do(ctx, "POST", taskPath(task, "answer"), map[string]string{"from": a.from, "text": text}, nil, callTimeout)
}

func (a *apiClient) Cancel(ctx context.Context, task, reason string) error {
	return a.do(ctx, "POST", taskPath(task, "cancel"), map[string]string{"from": a.from, "reason": reason}, nil, callTimeout)
}

func (a *apiClient) Done(ctx context.Context, task string) error {
	return a.do(ctx, "POST", "/v1/done", map[string]any{"task": task, "outcome": "done", "agent": "", "branch": "", "pr": "", "keep_session": false}, nil, callTimeout)
}

func (a *apiClient) SetModel(ctx context.Context, worker, model string) error {
	return a.do(ctx, "POST", "/v1/agents/"+url.PathEscape(worker)+"/model", map[string]any{"model": model, "reset": false}, nil, callTimeout)
}

func (a *apiClient) Access(ctx context.Context) (*fleetapi.AccessList, error) {
	var l fleetapi.AccessList
	if err := a.do(ctx, "GET", fleetapi.PathAccess, nil, &l, callTimeout); err != nil {
		return nil, err
	}
	return &l, nil
}

func (a *apiClient) ChangeAccess(ctx context.Context, c fleetapi.AccessChange) (*fleetapi.AccessList, error) {
	var l fleetapi.AccessList
	if err := a.do(ctx, "POST", fleetapi.PathAccess, c, &l, callTimeout); err != nil {
		return nil, err
	}
	return &l, nil
}
