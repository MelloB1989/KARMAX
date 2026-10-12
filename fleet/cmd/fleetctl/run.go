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
	_, pos, err := flags("run", args, func(fs *flag.FlagSet) {
		fs.StringVar(&repo, "repo", "", "")
		fs.StringVar(&agent, "agent", "", "")
		fs.StringVar(&task, "task", "", "")
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
	const usage = `run <brief…|-> [--repo R] [--agent A] [--task ID] [--from NAME] [--queue 30m] [--wait] [--timeout 2h] [--json] [--brief-file F]`
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
	req := map[string]string{"brief": brief, "repo": repo, "agent": agent, "from": from, "task": task}
	if queue > 0 {
		req["queue"] = queue.String()
		c.timeout = queue + 6*time.Minute
	}
	var rep daemon.RunReply
	if err := c.do("POST", "/v1/run", req, &rep); err != nil {
		var se *statusError
		if errors.As(err, &se) && se.Code == http.StatusConflict {
			var body struct {
				Candidates []daemon.Candidate `json:"candidates"`
			}
			_ = json.Unmarshal(se.Body, &body)
			fmt.Println(err)
			printCandidates(body.Candidates)
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
