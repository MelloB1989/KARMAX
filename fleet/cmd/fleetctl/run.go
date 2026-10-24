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
	from := sessionName()
	var repo, agent, task, briefFile, model string
	var queue, timeout time.Duration
	var wait, asJSON bool
	_, pos, err := flags("run", args, func(fs *flag.FlagSet) {
		fs.StringVar(&repo, "repo", "", "")
		fs.StringVar(&agent, "agent", "", "")
		fs.StringVar(&task, "task", "", "")
		fs.StringVar(&model, "model", "", "")
		fs.StringVar(&from, "from", from, "")
		fs.StringVar(&briefFile, "brief-file", "", "")
		fs.DurationVar(&queue, "queue", 0, "")
		fs.DurationVar(&timeout, "timeout", 2*time.Hour, "")
		fs.BoolVar(&wait, "wait", false, "")
		fs.BoolVar(&asJSON, "json", false, "")
	})
	if err != nil {
		return err
	}
	const usage = `run <brief…|-> [--repo R] [--agent A] [--model M] [--task ID] [--from NAME] [--queue 30m] [--wait] [--timeout 2h] [--json] [--brief-file F]`
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
	req := map[string]string{"brief": brief, "repo": repo, "agent": agent, "from": from, "task": task}
	if model != "" {
		req["model"] = model
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

// modelCmd is `model <agent> [<model>|--reset]`: GET or POST
// /v1/agents/<agent>/model.
func modelCmd(args []string) error {
	var reset, asJSON bool
	_, pos, err := flags("model", args, func(fs *flag.FlagSet) {
		fs.BoolVar(&reset, "reset", false, "")
		fs.BoolVar(&asJSON, "json", false, "")
	})
	if err != nil {
		return err
	}
	const usage = "model <agent> [<model>|--reset] [--json]"
	if len(pos) < 1 || len(pos) > 2 || (reset && len(pos) == 2) {
		return fmt.Errorf("usage: fleetctl %s", usage)
	}
	c, err := newClient()
	if err != nil {
		return err
	}
	path := "/v1/agents/" + url.PathEscape(pos[0]) + "/model"
	if reset || len(pos) == 2 {
		req := map[string]any{"reset": reset}
		if len(pos) == 2 {
			req["model"] = pos[1]
		}
		if err := c.do("POST", path, req, nil); err != nil {
			return err
		}
	}
	var rep daemon.ModelReply
	if err := c.do("GET", path, nil, &rep); err != nil {
		return err
	}
	if asJSON {
		return printJSON(rep)
	}
	or := func(s string) string {
		if s == "" {
			return "-"
		}
		return s
	}
	fmt.Printf("%s: default %s, override %s, task %s; live %s\n", rep.Agent, or(rep.Default), or(rep.Override), or(rep.Task), or(rep.Live))
	return nil
}
