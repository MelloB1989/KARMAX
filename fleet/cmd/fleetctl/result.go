package main

import (
	"errors"
	"flag"
	"fmt"
	"net/url"
	"os"
	"strings"

	"github.com/MelloB1989/karmax/fleet/internal/daemon"
)

// result is `fleetctl result <task>`: the task, its reports and its commits.
func result(args []string) error {
	var asJSON bool
	_, pos, err := flags("result", args, func(fs *flag.FlagSet) {
		fs.BoolVar(&asJSON, "json", false, "")
	})
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return errors.New("usage: fleetctl result <task> [--json]")
	}
	c, err := newClient()
	if err != nil {
		return err
	}
	var r daemon.TaskResult
	if err := c.do("GET", "/v1/tasks/"+url.PathEscape(pos[0]), nil, &r); err != nil {
		return err
	}
	if asJSON {
		return printJSON(r)
	}
	printResult(os.Stdout, r)
	return nil
}

func printResult(w *os.File, r daemon.TaskResult) {
	t := r.Task
	fmt.Fprintf(w, "task %s  agent %s  assigned by %s", t.ID, t.Agent, t.Assigner)
	if t.Repo != "" {
		fmt.Fprintf(w, "  repo %s", t.Repo)
	}
	fmt.Fprintln(w)
	fmt.Fprintf(w, "state %s", r.State)
	if t.Outcome != "" {
		fmt.Fprintf(w, " (%s)", t.Outcome)
	}
	if t.Branch != "" {
		fmt.Fprintf(w, "  branch %s", t.Branch)
	}
	if t.PR != "" {
		fmt.Fprintf(w, "  pr %s", t.PR)
	}
	fmt.Fprintln(w)
	if len(r.Reports) == 0 {
		fmt.Fprintln(w, "no reports")
	}
	for _, m := range r.Reports {
		fmt.Fprintf(w, "\n[%s] %s → %s:\n%s\n", m.At.Local().Format("2006-01-02 15:04:05"), m.From, m.To, strings.TrimRight(m.Text, "\n"))
	}
	if len(r.Commits) > 0 {
		fmt.Fprintf(w, "\n%d commit(s) ahead of main:\n", r.Ahead)
		for _, c := range r.Commits {
			fmt.Fprintf(w, "  %s\n", c)
		}
	}
	if r.CommitsError != "" {
		fmt.Fprintf(w, "\ncommits unavailable: %s\n", r.CommitsError)
	}
}
