package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/MelloB1989/karmax/fleet/internal/daemon"
)

// runCmd is `run <brief…>`: POST /v1/run, print where it went, and with
// --wait, wait on the task like `wait`. Which agent, the task id and the
// delivered text are fleetd's to decide.
func runCmd(args []string) error {
	from := os.Getenv("FLEET_NAME") // set in every fleet container
	if from == "" {
		from = os.Getenv("FLEET_AGENT")
	}
	var repo, agent, task, briefFile string
	var queue, timeout time.Duration
	var wait, asJSON bool
	var attach repeated
	_, pos, err := flags("run", args, func(fs *flag.FlagSet) {
		fs.StringVar(&repo, "repo", "", "")
		fs.StringVar(&agent, "agent", "", "")
		fs.StringVar(&task, "task", "", "")
		fs.StringVar(&from, "from", from, "")
		fs.StringVar(&briefFile, "brief-file", "", "")
		fs.DurationVar(&queue, "queue", 0, "")
		fs.DurationVar(&timeout, "timeout", 2*time.Hour, "")
		fs.BoolVar(&wait, "wait", false, "")
		fs.Var(&attach, "attach", "")
		fs.BoolVar(&asJSON, "json", false, "")
	})
	if err != nil {
		return err
	}
	const usage = `run <brief…|-> [--repo R] [--agent A] [--task ID] [--from NAME] [--queue 30m] [--attach P]… [--wait] [--timeout 2h] [--json] [--brief-file F]`
	var brief string
	switch {
	case briefFile != "":
		b, err := os.ReadFile(briefFile)
		if err != nil {
			return err
		}
		brief = string(b)
	case len(pos) == 1 && pos[0] == "-":
		b, err := io.ReadAll(os.Stdin)
		if err != nil {
			return err
		}
		brief = string(b)
	default:
		brief = strings.Join(pos, " ")
	}
	if strings.TrimSpace(brief) == "" {
		return fmt.Errorf("usage: fleetctl %s", usage)
	}
	c, err := newClient()
	if err != nil {
		return err
	}
	if task == "" {
		// Made here, not by fleetd: a run sent again after fleetd went away
		// is then the same run, and fleetd answers it with the agent it has.
		task = daemon.TaskID(brief, time.Now())
	}
	for _, p := range attach {
		if _, err := os.Lstat(p); err != nil {
			return err // before a task is placed
		}
	}
	req := map[string]any{"brief": brief, "repo": repo, "agent": agent, "from": from, "task": task}
	if len(attach) > 0 {
		req["attach"] = true // placed now, briefed once the files are in
	}
	if queue > 0 {
		req["queue"] = queue.String()
		c.timeout = queue + 6*time.Minute
	}
	var rep daemon.RunReply
	if err := doAgain(c, "POST", "/v1/run", req, &rep); err != nil {
		var se *statusError
		if errors.As(err, &se) && se.Code == http.StatusConflict {
			var body struct {
				Candidates []daemon.Candidate `json:"candidates"`
			}
			_ = json.Unmarshal(se.Body, &body)
			fmt.Println(err)
			if len(body.Candidates) > 0 { // none for a busy named agent, a member at their limit
				printCandidates(body.Candidates)
			}
			return exitCode{5}
		}
		return err
	}
	if len(attach) > 0 {
		if err := attachAndBrief(c, rep, attach, brief, repo, from); err != nil {
			return err
		}
	}
	if asJSON {
		_ = printJSON(rep)
	} else {
		fmt.Printf("task %s → %s (%s)\n", rep.Task, rep.Agent, rep.Why)
	}
	if !wait {
		return nil
	}
	// The task is new: every report filed under it is the caller's.
	r, err := waitFor(context.Background(), c, rep.Agent, rep.Task, 0, timeout, os.Stderr, sleepCtx)
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
			fmt.Printf("%s is %s\n", rep.Agent, r.Phase)
		case "timeout":
			fmt.Printf("no word from %s in %s\n", rep.Agent, timeout)
		}
	}
	switch r.Reason {
	case "phase":
		return exitCode{3}
	case "timeout":
		return exitCode{4}
	}
	return nil
}

// attachAndBrief copies the files into the task's inbox, then has fleetd
// deliver the brief. When the files fail to arrive the task is closed as
// undelivered: the agent was never told of it.
func attachAndBrief(c *client, rep daemon.RunReply, paths []string, brief, repo, from string) error {
	_, err := upload(context.Background(), c, rep.Agent, strings.TrimSuffix(daemon.Inbox(rep.Task), "/"), paths)
	if err == nil {
		err = c.do("POST", "/v1/tasks/"+url.PathEscape(rep.Task)+"/brief", map[string]string{"brief": brief, "repo": repo, "from": from}, nil)
		if err == nil {
			return nil
		}
	}
	if cerr := c.do("POST", "/v1/done", map[string]any{"task": rep.Task, "outcome": "undelivered", "keep_session": true}, nil); cerr != nil {
		err = fmt.Errorf("%w (and the task could not be closed: %v)", err, cerr)
	}
	return fmt.Errorf("task %s: attaching files: %w", rep.Task, err)
}

// runRetryFor is how long run keeps sending a run fleetd did not answer: a
// restart takes seconds, a reboot of its host a minute or two.
const runRetryFor = 3 * time.Minute

// runRetrySleep is the pause between tries; tests replace it.
var runRetrySleep = time.Sleep

// doAgain is c.do for a request that is the same however often it is sent (a
// run with its task id, done): while fleetd does not answer — restarting, its
// host rebooting — it is sent again, for up to runRetryFor.
func doAgain(c *client, method, path string, in, out any) error {
	err := c.do(method, path, in, out)
	for back, start := 2*time.Second, time.Now(); err != nil && retryable(err) && time.Since(start) < runRetryFor; back = min(back*2, 15*time.Second) {
		fmt.Fprintf(os.Stderr, "fleetd unreachable, sending %s again: %v\n", path, err)
		runRetrySleep(back)
		err = c.do(method, path, in, out)
	}
	return err
}

// pickCmd is `pick [--repo R]`: GET /v1/pick, as a table.
func pickCmd(args []string) error {
	var repo string
	var asJSON bool
	if _, _, err := flags("pick", args, func(fs *flag.FlagSet) {
		fs.StringVar(&repo, "repo", "", "")
		fs.BoolVar(&asJSON, "json", false, "")
	}); err != nil {
		return err
	}
	c, err := newClient()
	if err != nil {
		return err
	}
	var rep daemon.PickReply
	if err := c.do("GET", "/v1/pick?"+url.Values{"repo": {repo}}.Encode(), nil, &rep); err != nil {
		return err
	}
	if asJSON {
		_ = printJSON(rep)
	} else {
		printCandidates(rep.Candidates)
	}
	if rep.Agent == "" {
		return exitCode{5}
	}
	return nil
}

func printCandidates(cs []daemon.Candidate) {
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "AGENT\tOK\t5H\t7D\tIDLE\tWHY")
	for _, c := range cs {
		ok := "no"
		if c.Eligible {
			ok = "yes"
		}
		fmt.Fprintf(w, "%s\t%s\t%.0f%%\t%.0f%%\t%s\t%s\n", c.Agent, ok, c.FiveHour, c.SevenDay,
			daemon.Dur(time.Duration(c.IdleS)*time.Second), c.Why)
	}
	_ = w.Flush()
}
