// Command fleetctl is the agent fleet's CLI.
//
// Every command but attach is an HTTP client of fleetd: on a fleet host it
// finds fleetd through fleet.yaml; anywhere else through FLEETD_URL and
// FLEET_TOKEN, or the fleetd `fleetctl connect` saved, reached over the
// tailnet with no token (Tailscale says who you are). attach goes straight to the container through its host's exec
// prefix — it is your keyboard, and no model-callable tool ever wraps it.
// render, up and down run on a fleet host directly, and through fleetd from
// anywhere else (or with --remote).
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
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/MelloB1989/karmax/fleet/internal/agent"
	"github.com/MelloB1989/karmax/fleet/internal/archive"
	"github.com/MelloB1989/karmax/fleet/internal/config"
	"github.com/MelloB1989/karmax/fleet/internal/daemon"
	"github.com/MelloB1989/karmax/fleet/internal/hostctl"
	"github.com/MelloB1989/karmax/fleet/internal/ledger"
	"github.com/MelloB1989/karmax/fleet/internal/outbound"
	"github.com/MelloB1989/karmax/fleet/internal/recreate"
	"github.com/MelloB1989/karmax/fleet/internal/server"
	"github.com/MelloB1989/karmax/fleet/internal/target"
)

const usage = `fleetctl — the agent fleet (docs/AGENT-FLEET.md)

An <agent> is any worker: an agent's own name is its slot 1, <agent>:k its slot k (agent-01:2).

Roster and records (fleetd):
  status [--json]                     one row per worker (agent and slot), for the orchestrator
  ls [--json]                         the same, as a table, each agent's slots under it
  quota [--json]                      each subscription's windows
  events <agent> [--since 2h] [--json]
  usage [--by agent|model] [--since 7d] [--json]
  history <agent> [-n 20] [--json]
  tasks [--open] [--agent <name>] [-n 50] [--json]
  activity [--since 24h] [--bucket 15m] [--json]
  today [--json]                      tasks closed and spend since local midnight
  archive ls [--since 7d] [--json] | archive show <id> | archive prune --older 180d

Work (fleetd):
  run <brief…|-> [--repo R] [--agent A] [--task ID] [--from NAME] [--queue 30m] [--wait] [--timeout 2h] [--json] [--brief-file F]
                                      pick a free agent, record the task, deliver the brief; prints the
                                      task id and agent. Brief "-" reads stdin. --wait then waits for the
                                      report (exit 0; 3 down/cooling/disabled; 4 timeout); no free agent: exit 5
  pick [--repo R] [--json]            who a run would pick and why the others were passed over (exit 5: none free)
  give <agent> --task <id> <brief…>   record a task and deliver its brief; the agent reports to you
  assign <agent> --task <id> [--brief <text>] [--from <name>]
  inbox [--after <id>] [--follow] [--json]   what agents reported to the operator
  wait [<agent>] [--task <id>] [--after N] [--timeout 30m] [--json]
                      with --task the agent is optional; it returns the agent's first report after the
                      task's latest instruction (brief, follow-up), to whomever it was sent
  result <task> [--json]   the task's conversation (brief, reports, follow-ups) and its branch's commits
                                      until it reports, its task closes, or it stops
                                      (exit 0; 3 down/cooling/disabled; 4 timeout)
  done <agent> | --task <id> [--task <id>] [--outcome done] [--branch b] [--pr url] [--keep-session]
                                      --keep-session closes the task without archiving or rotating the session
  followup <task> <text…> [--from <name>] more instructions for the agent on an open task
  cancel <task> [--from <name>] [--reason <text>] interrupt the agent and close the task as cancelled
  tell <name> <text…> [--from <name>] message a session on another host
  rotate <agent> [--dry-run]          archive its session, start fresh (--dry-run says what it would close)
  restore <archive-id> [--agent <name>]
  enable <agent>                      after fixing a disabled subscription

Your keyboard:
  attach <agent> [--direct]           the live session's TUI (host only), through the console when
                                      the host runs one (one controller; give it back: Ctrl-])
  prompt <agent> <text…>              type into it, as you (fleetd)
  peek <agent> [-n 80]                its last lines (fleetd)

About fleetd:
  connect http://<host>:7878          from a machine on the tailnet: save that fleetd as yours, and
                                      say who it knows you as (no token: Tailscale says)
  whoami                              which scope this token has
  api                                 every route and the scopes it allows

The console (fleet-console on its host, reached over ssh; or FLEET_CONSOLE_URL + FLEET_CONSOLE_TOKEN):
  console snapshot [--json]           agents, tools, load, and every source with its age
  console watch [--json]              the snapshot, then patches, one JSON object per line
  console term <agent>                an agent's terminal bytes, raw, to stdout
  console health [--json]             the sources and their ages

Setup (on a fleet host directly, else through fleetd; --remote forces fleetd; up --when-idle and
the up plan are host-side):
  render [--out <dir>]                env files, compose files, briefs
  up [--dry-run|--plan] [--when-idle [--timeout 2h]] [<host>…] | down [<host>…]
                                      docker compose, per host. up first lists the containers it
                                      will recreate and why (image, env, brief); --dry-run stops there; --when-idle
                                      recreates each agent alone, once fleetd says it is not working
  image build [--tag T] [--src <repo>]  docker build of packaging/fleet/Dockerfile, its UID, GID, user,
                                      home and Claude Code version from fleet.yaml; prints the command first
  image ship <host> [--tag T]         docker save | docker --context <host> load (fleet host only)
`

func main() {
	if len(os.Args) < 2 || os.Args[1] == "-h" || os.Args[1] == "--help" || os.Args[1] == "help" {
		fmt.Print(usage)
		return
	}
	if err := run(os.Args[1], os.Args[2:]); err != nil {
		var ec exitCode
		if errors.As(err, &ec) {
			os.Exit(ec.code)
		}
		fmt.Fprintln(os.Stderr, "fleetctl:", err)
		os.Exit(1)
	}
}

// exitCode ends fleetctl with a code that says why, with nothing printed.
type exitCode struct{ code int }

func (e exitCode) Error() string { return fmt.Sprintf("exit %d", e.code) }

func configPath() string {
	if p := os.Getenv("FLEET_CONFIG"); p != "" {
		return p
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".karmax", "fleet", "fleet.yaml")
}

func loadConfig() (*config.Config, error) {
	c, err := config.Load(configPath())
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("no fleet.yaml at %s (set FLEET_CONFIG); this command runs on a fleet host", configPath())
	}
	return c, err
}

// client finds fleetd: FLEETD_URL and FLEET_TOKEN from the environment, else
// fleet.yaml's listen address and the env file's token, else (no fleet.yaml:
// not a fleet host) the fleetd connect saved, on the tailnet, with no token.
type client struct {
	base, token string
	timeout     time.Duration // 0: six minutes
}

func newClient() (*client, error) {
	c := &client{base: os.Getenv("FLEETD_URL"), token: os.Getenv("FLEET_TOKEN")}
	if _, err := os.Stat(configPath()); errors.Is(err, os.ErrNotExist) && c.token == "" {
		// Not a fleet host: the tailnet says who this is.
		if c.base == "" {
			c.base = savedServer()
		}
		if c.base == "" {
			return nil, fmt.Errorf("no fleetd to talk to: run fleetctl connect http://<fleetd host>:7878 (it is on your tailnet), or set FLEETD_URL")
		}
		c.base = strings.TrimRight(c.base, "/")
		return c, nil
	}
	if c.base == "" || c.token == "" {
		cfg, err := loadConfig()
		if err != nil {
			return nil, fmt.Errorf("set FLEETD_URL and FLEET_TOKEN, or %w", err)
		}
		if c.base == "" {
			c.base = "http://" + cfg.Listen
		}
		if c.token == "" {
			env, _ := outbound.ReadEnv(cfg.EnvFile)
			c.token = env[cfg.TokenEnv]
		}
	}
	c.base = strings.TrimRight(c.base, "/")
	return c, nil
}

func (c *client) do(method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, _ := json.Marshal(in)
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, c.base+path, body)
	if err != nil {
		return err
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	req.Header.Set("Content-Type", "application/json")
	timeout := c.timeout
	if timeout == 0 {
		timeout = 6 * time.Minute
	}
	// A connect fleetd does not answer (its host is down) fails in seconds,
	// not the minutes TCP would wait: callers retry, or queue (tell).
	tr := &http.Transport{Proxy: http.ProxyFromEnvironment, DialContext: (&net.Dialer{Timeout: 10 * time.Second}).DialContext}
	resp, err := (&http.Client{Timeout: timeout, Transport: tr}).Do(req)
	if err != nil {
		return fmt.Errorf("fleetd unreachable at %s: %w", c.base, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
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

// statusError is fleetd answering with an error status.
type statusError struct {
	Code int
	msg  string
	Body []byte // the response, for callers that read more than the message
}

func (e *statusError) Error() string { return e.msg }

// refused is an answer that retrying cannot change: the token is wrong.
func refused(err error) bool {
	var se *statusError
	return errors.As(err, &se) && (se.Code == http.StatusUnauthorized || se.Code == http.StatusForbidden)
}

// followInbox prints the operator's inbox after the given id, and keeps
// printing as it grows. A failed poll (fleetd restarting) is one line on
// errOut and a retry, 1 s doubling to 15 s, from the last id; only a refused
// token or ctx ends it.
func followInbox(ctx context.Context, c *client, after int64, asJSON bool, errOut io.Writer, sleep func(context.Context, time.Duration) error) error {
	const poll, minBack, maxBack = 2 * time.Second, time.Second, 15 * time.Second
	back := minBack
	for {
		var msgs []ledger.Relay
		if err := c.do("GET", fmt.Sprintf("/v1/inbox?after=%d", after), nil, &msgs); err != nil {
			if refused(err) {
				return err
			}
			fmt.Fprintln(errOut, "fleetd unreachable, retrying")
			if err := sleep(ctx, back); err != nil {
				return nil
			}
			back = min(back*2, maxBack)
			continue
		}
		back = minBack
		for _, m := range msgs {
			printMessage(m, asJSON)
			after = m.ID
		}
		if err := sleep(ctx, poll); err != nil {
			return nil
		}
	}
}

// retryable is a failed call another try may fix: fleetd not answering (it
// restarting) or a proxy's 502/503/504. Anything fleetd answered — a refused
// token, "no task T1" — stands.
func retryable(err error) bool {
	var se *statusError
	if !errors.As(err, &se) {
		return true
	}
	return se.Code == http.StatusBadGateway || se.Code == http.StatusServiceUnavailable || se.Code == http.StatusGatewayTimeout
}

// waitFor is `fleetctl wait`: from the inbox position after (-1: the end, now)
// until the agent reports, its task closes or it stops. fleetd restarting
// mid-wait is a line on errOut and a retry, 1 s doubling to 15 s, within the
// one timeout; only an answer (refused token, no such task) ends it early.
func waitFor(ctx context.Context, c *client, agent, task string, after int64, timeout time.Duration, errOut io.Writer, sleep func(context.Context, time.Duration) error) (daemon.WaitResult, error) {
	const minBack, maxBack = time.Second, 15 * time.Second
	deadline := time.Now().Add(timeout)
	back := minBack
	retry := func(err error) (bool, error) { // true: try again
		if !retryable(err) {
			return false, err
		}
		if !time.Now().Add(back).Before(deadline) {
			return false, nil // no time left for another try: the wait timed out
		}
		fmt.Fprintln(errOut, "fleetd unreachable, retrying")
		if err := sleep(ctx, back); err != nil {
			return false, err
		}
		back = min(back*2, maxBack)
		return true, nil
	}
	if after < 0 { // only what arrives from now on
		for {
			var msgs []ledger.Relay
			err := c.do("GET", "/v1/inbox", nil, &msgs)
			if err == nil {
				after = 0
				if len(msgs) > 0 {
					after = msgs[len(msgs)-1].ID
				}
				break
			}
			if again, err := retry(err); !again {
				if err != nil {
					return daemon.WaitResult{}, err
				}
				return daemon.WaitResult{Reason: "timeout"}, nil
			}
		}
	}
	left := timeout
	for first := true; ; first = false {
		if !first { // a retry waits only for what is left of the one timeout
			if left = time.Until(deadline); left <= 0 {
				return daemon.WaitResult{Reason: "timeout"}, nil
			}
		}
		c.timeout = left + 30*time.Second
		q := url.Values{"task": {task}, "after": {fmt.Sprint(after)}, "timeout": {left.Round(time.Millisecond).String()}}
		if agent != "" {
			q.Set("agent", agent)
		}
		var r daemon.WaitResult
		err := c.do("GET", "/v1/wait?"+q.Encode(), nil, &r)
		if err == nil {
			return r, nil
		}
		if again, err := retry(err); !again {
			if err != nil {
				return daemon.WaitResult{}, err
			}
			return daemon.WaitResult{Reason: "timeout"}, nil
		}
	}
}

func printJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// flags parses a subcommand's flags, allowing them after positional args.
func flags(name string, args []string, def func(*flag.FlagSet)) (*flag.FlagSet, []string, error) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	def(fs)
	var pos []string
	for len(args) > 0 {
		if err := fs.Parse(args); err != nil {
			return nil, nil, err
		}
		args = fs.Args()
		if len(args) > 0 {
			pos = append(pos, args[0])
			args = args[1:]
		}
	}
	return fs, pos, nil
}

func need(pos []string, n int, what string) error {
	if len(pos) < n {
		return fmt.Errorf("usage: fleetctl %s", what)
	}
	return nil
}

func run(cmd string, args []string) error {
	switch cmd {
	case "status", "ls":
		var asJSON bool
		if _, _, err := flags(cmd, args, func(fs *flag.FlagSet) { fs.BoolVar(&asJSON, "json", false, "") }); err != nil {
			return err
		}
		c, err := newClient()
		if err != nil {
			return err
		}
		var out struct {
			Agents       []daemon.Row    `json:"agents"`
			Orchestrator *outbound.Quota `json:"orchestrator"`
		}
		if err := c.do("GET", "/v1/status", nil, &out); err != nil {
			return err
		}
		if asJSON || cmd == "status" && !isTerminal() {
			return printJSON(out)
		}
		return table(out.Agents, out.Orchestrator)

	case "quota":
		return getAndPrint(args, "/v1/quota", func(raw json.RawMessage) error {
			var q map[string]ledger.Quota
			if err := json.Unmarshal(raw, &q); err != nil {
				return err
			}
			w := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
			fmt.Fprintln(w, "ACCOUNT\t5H%\tRESETS\t7D%\tRESETS\tAS OF\tSOURCE")
			for acct, x := range q {
				fmt.Fprintf(w, "%s\t%.0f\t%s\t%.0f\t%s\t%s\t%s\n", acct, x.FiveHour, clock(x.FiveResets), x.SevenDay,
					clock(x.SevenResets), clock(x.At), x.Source)
			}
			return w.Flush()
		})

	case "events":
		var since string
		var asJSON bool
		_, pos, err := flags(cmd, args, func(fs *flag.FlagSet) {
			fs.StringVar(&since, "since", "2h", "")
			fs.BoolVar(&asJSON, "json", false, "")
		})
		if err != nil {
			return err
		}
		agentName := ""
		if len(pos) > 0 {
			agentName = pos[0]
		}
		c, err := newClient()
		if err != nil {
			return err
		}
		var evs []ledger.Event
		if err := c.do("GET", "/v1/events?agent="+url.QueryEscape(agentName)+"&since="+url.QueryEscape(since), nil, &evs); err != nil {
			return err
		}
		if asJSON {
			return printJSON(evs)
		}
		for _, e := range evs {
			fmt.Printf("%s  %-9s %-24s %s %s\n", e.At.Local().Format("01-02 15:04:05"), e.Agent, e.Kind, short(e.Session), e.Detail)
		}
		return nil

	case "usage":
		var by, since string
		var asJSON bool
		if _, _, err := flags(cmd, args, func(fs *flag.FlagSet) {
			fs.StringVar(&by, "by", "agent", "")
			fs.StringVar(&since, "since", "7d", "")
			fs.BoolVar(&asJSON, "json", false, "")
		}); err != nil {
			return err
		}
		c, err := newClient()
		if err != nil {
			return err
		}
		var rows []ledger.UsageRow
		if err := c.do("GET", "/v1/usage?by="+url.QueryEscape(by)+"&since="+url.QueryEscape(since), nil, &rows); err != nil {
			return err
		}
		if asJSON {
			return printJSON(rows)
		}
		w := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
		fmt.Fprintf(w, "%s\tREQUESTS\tINPUT\tOUTPUT\tCACHE READ\tCACHE WRITE\tLIST PRICE $\n", strings.ToUpper(by))
		for _, r := range rows {
			fmt.Fprintf(w, "%s\t%d\t%d\t%d\t%d\t%d\t%.2f\n", r.Key, r.Requests, r.Input, r.Output, r.CacheRead, r.CacheCreation, r.CostUSD)
		}
		fmt.Fprintln(w, "(list-price estimate; subscriptions are not billed per token)")
		return w.Flush()

	case "history":
		var n int
		var asJSON bool
		_, pos, err := flags(cmd, args, func(fs *flag.FlagSet) {
			fs.IntVar(&n, "n", 20, "")
			fs.BoolVar(&asJSON, "json", false, "")
		})
		if err != nil {
			return err
		}
		if err := need(pos, 1, "history <agent>"); err != nil {
			return err
		}
		c, err := newClient()
		if err != nil {
			return err
		}
		var rows []ledger.Session
		if err := c.do("GET", fmt.Sprintf("/v1/history?agent=%s&n=%d", url.QueryEscape(pos[0]), n), nil, &rows); err != nil {
			return err
		}
		if asJSON {
			return printJSON(rows)
		}
		w := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
		fmt.Fprintln(w, "ENDED\tSESSION\tTASK\tREASON\tTURNS\tOUTPUT\tARCHIVE")
		for _, s := range rows {
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%d\t%d\t%s\n", s.Ended.Local().Format("01-02 15:04"), short(s.ID), dash(s.Task),
				s.Reason, s.Turns, s.Output, dash(s.ArchiveID))
		}
		return w.Flush()

	case "archive":
		return archiveCmd(args)

	case "assign", "give":
		var task, brief, from string
		_, pos, err := flags(cmd, args, func(fs *flag.FlagSet) {
			fs.StringVar(&task, "task", "", "")
			fs.StringVar(&brief, "brief", "", "")
			fs.StringVar(&from, "from", "operator", "")
		})
		if err != nil {
			return err
		}
		use := "assign <agent> --task <id> [--brief <text>] [--from <name>]"
		if cmd == "give" {
			use = "give <agent> --task <id> <brief…>"
			brief = strings.Join(pos[min(1, len(pos)):], " ")
			if strings.TrimSpace(brief) == "" {
				return errors.New("usage: fleetctl " + use)
			}
			brief = fmt.Sprintf("Task %s from %s. %s", task, from, brief)
		}
		if err := need(pos, 1, use); err != nil {
			return err
		}
		if task == "" {
			return errors.New("usage: fleetctl " + use)
		}
		return post("/v1/assign", map[string]string{"agent": pos[0], "task": task, "from": from, "brief": brief})

	case "inbox":
		var after int64
		var follow, asJSON bool
		if _, _, err := flags(cmd, args, func(fs *flag.FlagSet) {
			fs.Int64Var(&after, "after", 0, "")
			fs.BoolVar(&follow, "follow", false, "")
			fs.BoolVar(&asJSON, "json", false, "")
		}); err != nil {
			return err
		}
		c, err := newClient()
		if err != nil {
			return err
		}
		if follow {
			ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			return followInbox(ctx, c, after, asJSON, os.Stderr, sleepCtx)
		}
		var msgs []ledger.Relay
		if err := c.do("GET", fmt.Sprintf("/v1/inbox?after=%d", after), nil, &msgs); err != nil {
			return err
		}
		for _, m := range msgs {
			printMessage(m, asJSON)
		}
		return nil

	case "wait":
		var task string
		var timeout time.Duration
		var asJSON bool
		after := int64(-1)
		_, pos, err := flags(cmd, args, func(fs *flag.FlagSet) {
			fs.StringVar(&task, "task", "", "")
			fs.DurationVar(&timeout, "timeout", 30*time.Minute, "")
			fs.Int64Var(&after, "after", -1, "")
			fs.BoolVar(&asJSON, "json", false, "")
		})
		if err != nil {
			return err
		}
		agent := ""
		if len(pos) > 0 {
			agent = pos[0]
		}
		if agent == "" && task == "" {
			return errors.New("usage: fleetctl wait [<agent>] [--task <id>] [--after N] [--timeout 30m]")
		}
		if task != "" && after < 0 {
			after = 0 // the server resolves it: the answer to the latest instruction
		}
		c, err := newClient()
		if err != nil {
			return err
		}
		who := agent
		if who == "" {
			who = "task " + task
		}
		r, err := waitFor(context.Background(), c, agent, task, after, timeout, os.Stderr, sleepCtx)
		if err != nil {
			return err
		}
		if asJSON {
			_ = printJSON(r)
		} else {
			switch r.Reason {
			case "message":
				printMessage(*r.Message, false)
			case "closed":
				fmt.Printf("task %s closed: %s %s\n", r.Task.ID, r.Task.Outcome, r.Task.Branch)
			case "phase":
				fmt.Printf("%s is %s\n", who, r.Phase)
			case "timeout":
				fmt.Printf("no word from %s in %s\n", who, timeout)
			}
		}
		switch r.Reason {
		case "phase":
			return exitCode{3}
		case "timeout":
			return exitCode{4}
		}
		return nil

	case "result":
		return result(args)

	case "guide":
		return guideCmd(args)

	case "skill":
		return skillCmd(args)

	case "connect":
		return connectCmd(args)

	case "whoami":
		return getAndPrint(args, "/v1/whoami", func(raw json.RawMessage) error {
			var w struct{ Scope, Name string }
			_ = json.Unmarshal(raw, &w)
			fmt.Printf("%s (scope %s)\n", w.Name, w.Scope)
			return nil
		})

	case "api":
		return getAndPrint(args, "/v1", func(raw json.RawMessage) error {
			var idx []server.Route
			if err := json.Unmarshal(raw, &idx); err != nil {
				return err
			}
			w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
			for _, r := range idx {
				fmt.Fprintf(w, "%s\t%s\n", r.Route, strings.Join(r.Scopes, ","))
			}
			return w.Flush()
		})

	case "done":
		var tasks repeated
		var outcome, branch, pr string
		var keep bool
		_, pos, err := flags(cmd, args, func(fs *flag.FlagSet) {
			fs.BoolVar(&keep, "keep-session", false, "")
			fs.Var(&tasks, "task", "")
			fs.StringVar(&outcome, "outcome", "done", "")
			fs.StringVar(&branch, "branch", "", "")
			fs.StringVar(&pr, "pr", "", "")
		})
		if err != nil {
			return err
		}
		agent := ""
		if len(pos) > 0 {
			agent = pos[0]
		} else if len(tasks) == 0 {
			return errors.New("usage: fleetctl done <agent> [--task <id>] [--keep-session] | done --task <id> [--task <id>…]")
		}
		if len(tasks) <= 1 {
			c, err := newClient()
			if err != nil {
				return err
			}
			if err := doAgain(c, "POST", "/v1/done", map[string]any{"agent": agent, "task": tasks.first(), "outcome": outcome, "branch": branch, "pr": pr, "keep_session": keep}, nil); err != nil {
				return err
			}
			fmt.Println("ok")
			return nil
		}
		// Several tasks: each is closed by its own id, so an agent, branch or PR would apply to all of them.
		if agent != "" || branch != "" || pr != "" {
			return errors.New("done with several --task closes each by its id alone: drop the agent, --branch and --pr, or close one task at a time")
		}
		c, err := newClient()
		if err != nil {
			return err
		}
		var failed []error
		for _, id := range tasks {
			if err := doAgain(c, "POST", "/v1/done", map[string]any{"task": id, "outcome": outcome, "agent": "", "branch": "", "pr": "", "keep_session": keep}, nil); err != nil {
				failed = append(failed, fmt.Errorf("%s: %w", id, err))
				continue
			}
			fmt.Println("ok " + id)
		}
		return errors.Join(failed...)

	case "run":
		return runCmd(args)
	case "pick":
		return pickCmd(args)
	case "followup", "cancel":
		return followupCmd(cmd, args)

	case "tell":
		from := os.Getenv("FLEET_NAME") // set in every fleet container
		if from == "" {
			from = os.Getenv("FLEET_AGENT")
		}
		if from == "" {
			from = "operator"
		}
		_, pos, err := flags(cmd, args, func(fs *flag.FlagSet) { fs.StringVar(&from, "from", from, "") })
		if err != nil {
			return err
		}
		if err := need(pos, 2, `tell <name> "<text>"`); err != nil {
			return err
		}
		c, err := newClient()
		if err != nil {
			return err
		}
		said, err := tell(c, from, pos[0], strings.Join(pos[1:], " "), outboxDir(), time.Sleep)
		if err != nil {
			return err
		}
		fmt.Println(said)
		return nil

	case "enable":
		if err := need(args, 1, cmd+" <agent>"); err != nil {
			return err
		}
		return post("/v1/"+cmd, map[string]string{"agent": args[0]})

	case "rotate":
		var dry bool
		_, pos, err := flags(cmd, args, func(fs *flag.FlagSet) { fs.BoolVar(&dry, "dry-run", false, "") })
		if err != nil {
			return err
		}
		if err := need(pos, 1, "rotate <agent> [--dry-run]"); err != nil {
			return err
		}
		c, err := newClient()
		if err != nil {
			return err
		}
		var out struct {
			Closes string `json:"closes"`
			When   string `json:"when"`
		}
		if err := c.do("POST", "/v1/rotate", map[string]any{"agent": pos[0], "dry_run": dry}, &out); err != nil {
			return err
		}
		verb := "rotating"
		if dry {
			verb = "would rotate (dry run, nothing changed)"
		}
		task := "no open task"
		if out.Closes != "" {
			task = "closes task " + out.Closes + " as rotated"
		}
		fmt.Printf("%s %s: %s; takes effect at %s\n", verb, pos[0], task, out.When)
		return nil

	case "tasks":
		var open, asJSON bool
		var agentName string
		var n int
		if _, _, err := flags(cmd, args, func(fs *flag.FlagSet) {
			fs.BoolVar(&open, "open", false, "")
			fs.StringVar(&agentName, "agent", "", "")
			fs.IntVar(&n, "n", 0, "")
			fs.BoolVar(&asJSON, "json", false, "")
		}); err != nil {
			return err
		}
		q := url.Values{"agent": {agentName}, "n": {fmt.Sprint(n)}}
		if open {
			q.Set("open", "1")
		}
		return getAndPrint(jsonFlag(asJSON), "/v1/tasks?"+q.Encode(), func(raw json.RawMessage) error {
			var ts []ledger.Task
			if err := json.Unmarshal(raw, &ts); err != nil {
				return err
			}
			w := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
			fmt.Fprintln(w, "TASK\tAGENT\tFROM\tASSIGNED\tCLOSED\tOUTCOME")
			for _, t := range ts {
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n", t.ID, t.Agent, dash(t.Assigner), clock(t.Assigned), clock(t.Closed), dash(t.Outcome))
			}
			return w.Flush()
		})

	case "activity":
		var since, bucket string
		var asJSON bool
		if _, _, err := flags(cmd, args, func(fs *flag.FlagSet) {
			fs.StringVar(&since, "since", "24h", "")
			fs.StringVar(&bucket, "bucket", "15m", "")
			fs.BoolVar(&asJSON, "json", false, "")
		}); err != nil {
			return err
		}
		q := url.Values{"since": {since}, "bucket": {bucket}}
		return getAndPrint(jsonFlag(asJSON), "/v1/activity?"+q.Encode(), func(raw json.RawMessage) error {
			var a struct {
				BucketS int                        `json:"bucket_s"`
				Agents  map[string][]ledger.Bucket `json:"agents"`
			}
			if err := json.Unmarshal(raw, &a); err != nil {
				return err
			}
			w := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
			fmt.Fprintf(w, "AGENT\tEVENTS\tBUSIEST BUCKET (%ds)\n", a.BucketS)
			for name, bs := range a.Agents {
				total, best := 0, ledger.Bucket{}
				for _, b := range bs {
					total += b.N
					if b.N > best.N {
						best = b
					}
				}
				fmt.Fprintf(w, "%s\t%d\t%s (%d)\n", name, total, clock(best.T), best.N)
			}
			return w.Flush()
		})

	case "today":
		var asJSON bool
		if _, _, err := flags(cmd, args, func(fs *flag.FlagSet) { fs.BoolVar(&asJSON, "json", false, "") }); err != nil {
			return err
		}
		return getAndPrint(jsonFlag(asJSON), "/v1/today", func(raw json.RawMessage) error {
			var t struct {
				Since        time.Time `json:"since"`
				TasksClosed  int       `json:"tasks_closed"`
				SpendUSD     float64   `json:"spend_usd"`
				OutputTokens int64     `json:"output_tokens"`
			}
			if err := json.Unmarshal(raw, &t); err != nil {
				return err
			}
			fmt.Printf("since %s: %d tasks closed, $%.2f (list price), %d output tokens\n",
				t.Since.Local().Format("Mon 15:04"), t.TasksClosed, t.SpendUSD, t.OutputTokens)
			return nil
		})

	case "restore":
		var onto string
		_, pos, err := flags(cmd, args, func(fs *flag.FlagSet) { fs.StringVar(&onto, "agent", "", "") })
		if err != nil {
			return err
		}
		if err := need(pos, 1, "restore <archive-id> [--agent <name>]"); err != nil {
			return err
		}
		return post("/v1/restore", map[string]string{"archive": pos[0], "agent": onto})

	case "console":
		return consoleCmd(args)

	case "attach":
		return keyboard(cmd, args)

	case "peek":
		var n int
		_, pos, err := flags(cmd, args, func(fs *flag.FlagSet) { fs.IntVar(&n, "n", 80, "") })
		if err != nil {
			return err
		}
		if err := need(pos, 1, "peek <agent> [-n 80]"); err != nil {
			return err
		}
		c, err := newClient()
		if err != nil {
			return err
		}
		var out struct{ Text string }
		if err := c.do("GET", fmt.Sprintf("/v1/peek?agent=%s&n=%d", url.QueryEscape(pos[0]), n), nil, &out); err != nil {
			return err
		}
		fmt.Print(out.Text)
		return nil

	case "prompt":
		if err := need(args, 2, `prompt <agent> "<text>"`); err != nil {
			return err
		}
		return post("/v1/prompt", map[string]string{"agent": args[0], "text": strings.Join(args[1:], " ")})

	case "render":
		return hostCmd(cmd, args)

	case "up", "down":
		return hostUpDown(cmd, args)

	case "image":
		return imageCmd(args)
	}
	return fmt.Errorf("unknown command %q (fleetctl help)", cmd)
}

func printMessage(m ledger.Relay, asJSON bool) {
	if asJSON {
		b, _ := json.Marshal(m)
		fmt.Println(string(b))
		return
	}
	fmt.Printf("#%d %s %s: %s\n", m.ID, m.At.Local().Format("15:04:05"), m.From, m.Text)
}

func post(path string, body any) error {
	c, err := newClient()
	if err != nil {
		return err
	}
	var raw json.RawMessage
	if err := c.do("POST", path, body, &raw); err != nil {
		return err
	}
	// A message fleetd holds for a receiver it cannot reach is taken: say so.
	var rep sendReply
	if json.Unmarshal(raw, &rep) == nil && rep.Queued {
		fmt.Println(rep.Why)
		return nil
	}
	fmt.Println("ok")
	return nil
}

func getAndPrint(args []string, path string, human func(json.RawMessage) error) error {
	var asJSON bool
	if _, _, err := flags("get", args, func(fs *flag.FlagSet) { fs.BoolVar(&asJSON, "json", false, "") }); err != nil {
		return err
	}
	c, err := newClient()
	if err != nil {
		return err
	}
	var raw json.RawMessage
	if err := c.do("GET", path, nil, &raw); err != nil {
		return err
	}
	if asJSON {
		_, err := os.Stdout.Write(append(raw, '\n'))
		return err
	}
	return human(raw)
}

func archiveCmd(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: fleetctl archive ls|show|prune")
	}
	switch args[0] {
	case "ls":
		var since string
		var asJSON bool
		if _, _, err := flags("archive ls", args[1:], func(fs *flag.FlagSet) {
			fs.StringVar(&since, "since", "", "")
			fs.BoolVar(&asJSON, "json", false, "")
		}); err != nil {
			return err
		}
		return getAndPrint(jsonFlag(asJSON), "/v1/archives?since="+url.QueryEscape(since), func(raw json.RawMessage) error {
			var all []*archive.Manifest
			if err := json.Unmarshal(raw, &all); err != nil {
				return err
			}
			w := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
			fmt.Fprintln(w, "ARCHIVE\tTASK\tREASON\tTURNS\tKEPT WORKTREES\tLAST")
			for _, m := range all {
				fmt.Fprintf(w, "%s\t%s\t%s\t%d\t%d\t%s\n", m.ID, dash(m.Task), m.Reason, m.Turns, len(m.KeptWorktrees()), clip(m.LastAssistant, 60))
			}
			return w.Flush()
		})
	case "show":
		if err := need(args, 2, "archive show <id>"); err != nil {
			return err
		}
		c, err := newClient()
		if err != nil {
			return err
		}
		var raw json.RawMessage
		if err := c.do("GET", "/v1/archives/"+args[1], nil, &raw); err != nil {
			return err
		}
		_, err = os.Stdout.Write(append(raw, '\n'))
		return err
	case "prune":
		var older string
		if _, _, err := flags("archive prune", args[1:], func(fs *flag.FlagSet) { fs.StringVar(&older, "older", "", "") }); err != nil {
			return err
		}
		if older == "" {
			return errors.New("usage: fleetctl archive prune --older 180d (archives are otherwise kept forever)")
		}
		c, err := newClient()
		if err != nil {
			return err
		}
		var out struct{ Removed int }
		if err := c.do("DELETE", "/v1/archives?older="+url.QueryEscape(older), nil, &out); err != nil {
			return err
		}
		fmt.Printf("pruned %d archives older than %s\n", out.Removed, older)
		return nil
	}
	return fmt.Errorf("unknown archive command %q", args[0])
}

func jsonFlag(on bool) []string {
	if on {
		return []string{"--json"}
	}
	return nil
}

// onFleetHost is whether this machine has a fleet.yaml, so it can run
// render, up and down itself rather than ask fleetd to.
func onFleetHost() bool {
	_, err := os.Stat(configPath())
	return err == nil
}

// hostCmd is render: directly on a fleet host, else (or with --remote)
// through fleetd.
func hostCmd(cmd string, args []string) error {
	var out string
	var remote bool
	_, _, err := flags(cmd, args, func(fs *flag.FlagSet) {
		fs.StringVar(&out, "out", "", "")
		fs.BoolVar(&remote, "remote", false, "")
	})
	if err != nil {
		return err
	}
	if onFleetHost() && !remote {
		cfg, err := loadConfig()
		if err != nil {
			return err
		}
		return hostctl.Render(cfg, out, os.Stdout)
	}
	if out != "" {
		return errors.New("--out writes on this host; run render here, where fleet.yaml is")
	}
	return askFleetd("render", nil)
}

// imageRun and imagePipe run the docker commands of `image`; tests replace them.
var (
	imageRun  hostctl.Runner = hostctl.ExecRunner
	imagePipe hostctl.Piper  = hostctl.ExecPiper
)

// imageCmd is `image build` and `image ship`: docker next to fleet.yaml, so
// the image's identity comes from there and never from typed build args.
func imageCmd(args []string) error {
	const use = "image build [--tag T] [--src <repo>] | image ship <host> [--tag T]"
	if len(args) == 0 {
		return errors.New("usage: fleetctl " + use)
	}
	var tag, src string
	_, pos, err := flags("image "+args[0], args[1:], func(fs *flag.FlagSet) {
		fs.StringVar(&tag, "tag", "", "")
		fs.StringVar(&src, "src", "", "")
	})
	if err != nil {
		return err
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	ctx := context.Background()
	switch args[0] {
	case "build":
		if len(pos) != 0 {
			return errors.New("usage: fleetctl " + use)
		}
		if src == "" {
			src = "."
		}
		if src, err = hostctl.FindSource(src); err != nil {
			return err
		}
		return hostctl.ImageBuild(ctx, cfg, src, tag, os.Stdout, imageRun)
	case "ship":
		if len(pos) != 1 {
			return errors.New("usage: fleetctl " + use)
		}
		return hostctl.ImageShip(ctx, cfg, pos[0], tag, os.Stdout, imagePipe)
	}
	return errors.New("usage: fleetctl " + use)
}

// hostUpDown is up and down: on a fleet host, upDown (with its plan,
// --dry-run and --when-idle); with --remote or off a fleet host, fleetd's
// route, which brings hosts up or down without the plan.
func hostUpDown(cmd string, args []string) error {
	remote := false
	var rest []string
	for _, a := range args {
		if a == "--remote" || a == "-remote" {
			remote = true
			continue
		}
		rest = append(rest, a)
	}
	if onFleetHost() && !remote {
		return upDown(cmd, rest)
	}
	for _, a := range rest {
		if strings.HasPrefix(a, "-") {
			return fmt.Errorf("%s needs a fleet host (the plan and --when-idle run next to docker); here %s goes through fleetd, hosts only", a, cmd)
		}
	}
	return askFleetd(cmd, rest)
}

func askFleetd(route string, hosts []string) error {
	c, err := newClient()
	if err != nil {
		return err
	}
	var reply struct{ Output string }
	if err := c.do("POST", "/v1/"+route, map[string]any{"hosts": hosts}, &reply); err != nil {
		return err
	}
	fmt.Print(reply.Output)
	return nil
}

// keyboard is attach: straight to the container, or through the console.
func keyboard(cmd string, args []string) error {
	var direct bool
	_, pos, err := flags(cmd, args, func(fs *flag.FlagSet) { fs.BoolVar(&direct, "direct", false, "") })
	if err != nil {
		return err
	}
	if err := need(pos, 1, "attach <agent> [--direct]"); err != nil {
		return err
	}
	if !direct && os.Getenv("FLEET_CONSOLE_URL") != "" {
		return attachViaConsole(pos[0])
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	a, ok := cfg.Worker(pos[0])
	if !ok {
		return fmt.Errorf("no agent %q", pos[0])
	}
	if !direct && consoleFor(a.Host) {
		return attachViaConsole(pos[0])
	}
	ag := &agent.Agent{Name: a.Name, Slot: a.Slot, C: target.Container{Exec: cfg.Hosts[a.Host].Exec, Name: a.Container}}
	argv := ag.AttachArgv()
	c := exec.Command(argv[0], argv[1:]...)
	c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
	return c.Run()
}

// attachViaConsole: the console owns the one-controller rule, so attach
// goes through it whenever the agent's host runs one. --direct bypasses it.
func attachViaConsole(agentName string) error {
	ctx := context.Background()
	c, err := newConsole(ctx)
	if err != nil {
		return fmt.Errorf("%w (attach --direct goes to the container without the console's one-controller rule)", err)
	}
	return consoleAttach(ctx, c, agentName)
}

func table(rows []daemon.Row, orch *outbound.Quota) error {
	w := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(w, "AGENT\tHOST\tSTATE\tTASK\tIDLE\t5H%\t7D%\tRESET\tCTX%\tMODELS")
	for _, r := range rows {
		state := r.State
		if r.Low {
			state += ",low"
		}
		if r.Reserved {
			state += ",reserved"
		}
		if r.Reason != "" {
			state += " (" + r.Reason + ")"
		}
		name := r.Agent
		if r.Slot > 1 {
			name = "  " + name // under its agent's slot 1
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%.0f\t%.0f\t%s\t%.0f\t%s\n", name, r.Host, state, dash(r.Task), dash(r.Idle),
			r.FiveHour, r.SevenDay, clock(r.FiveResets), r.Context, strings.Join(r.Models, ","))
	}
	if orch != nil {
		fmt.Fprintf(w, "karmax\t-\torchestrator\t-\t-\t%.0f\t%.0f\t%s\t-\t-\n", orch.FiveHour, orch.SevenDay, clock(orch.FiveHourResets))
	}
	return w.Flush()
}

func isTerminal() bool {
	st, err := os.Stdout.Stat()
	return err == nil && st.Mode()&os.ModeCharDevice != 0
}

func clock(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	if time.Until(t) > 20*time.Hour || time.Since(t) > 20*time.Hour {
		return t.Local().Format("Mon 15:04")
	}
	return t.Local().Format("15:04")
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func short(s string) string {
	if len(s) > 8 {
		return s[:8]
	}
	return dash(s)
}

func clip(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}

// upDown is `fleetctl up|down [--when-idle [--timeout d]] [host…]`. Up first
// says which containers it will recreate and why; with --when-idle it
// recreates the agents among them one at a time, each once fleetd says it is
// not working.
func upDown(cmd string, args []string) error {
	var whenIdle, dryRun, plan bool
	var timeout time.Duration
	_, hosts, err := flags(cmd, args, func(fs *flag.FlagSet) {
		if cmd == "up" {
			fs.BoolVar(&dryRun, "dry-run", false, "")
			fs.BoolVar(&plan, "plan", false, "")
			fs.BoolVar(&whenIdle, "when-idle", false, "")
			fs.DurationVar(&timeout, "timeout", 2*time.Hour, "")
		}
	})
	if err != nil {
		return err
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	if cmd == "up" {
		if err := renderCurrent(cfg, configPath()); err != nil {
			return err
		}
	}
	ctx := context.Background()
	if whenIdle && timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	return hostctl.Update(ctx, cfg, cmd == "up", hosts, hostctl.Options{
		Run: hostctl.ExecRunner, Docker: runDocker, DryRun: dryRun || plan, WhenIdle: whenIdle,
		States: fleetdStates, Sleep: sleepCtx, Every: 10 * time.Second,
	}, os.Stdout)
}

// renderCurrent fails when fleet.yaml or its env file changed after the last
// render: up would roll out the old compose files, and its plan would compare
// against them too.
func renderCurrent(cfg *config.Config, yamlPath string) error {
	files, _ := filepath.Glob(filepath.Join(hostctl.RenderDir(cfg), "compose.*.yaml"))
	var rendered time.Time
	for _, f := range files {
		if st, err := os.Stat(f); err == nil && (rendered.IsZero() || st.ModTime().Before(rendered)) {
			rendered = st.ModTime()
		}
	}
	if rendered.IsZero() {
		return nil // never rendered: up says so itself
	}
	for _, src := range []string{yamlPath, cfg.EnvFile} {
		if st, err := os.Stat(src); err == nil && st.ModTime().After(rendered) {
			return fmt.Errorf("%s changed after the last render: run fleetctl render, then fleetctl up", src)
		}
	}
	return nil
}

// runDocker runs the docker CLI for the plan; tests replace it.
var runDocker recreate.Runner = hostctl.RealDocker

// fleetdStates is each agent's state, from fleetd's status.
func fleetdStates(ctx context.Context) (map[string]string, error) {
	c, err := newClient()
	if err != nil {
		return nil, err
	}
	var out struct {
		Agents []daemon.Row `json:"agents"`
	}
	if err := c.do("GET", "/v1/status", nil, &out); err != nil {
		return nil, err
	}
	m := map[string]string{}
	for _, r := range out.Agents {
		m[r.Agent] = r.State
	}
	return m, nil
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	select {
	case <-time.After(d):
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// repeated is a flag that may be given more than once (--task A --task B).
type repeated []string

func (r *repeated) String() string     { return strings.Join(*r, ",") }
func (r *repeated) Set(v string) error { *r = append(*r, v); return nil }

func (r repeated) first() string {
	if len(r) == 0 {
		return ""
	}
	return r[0]
}
