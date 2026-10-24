package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/MelloB1989/karmax/fleet/internal/daemon"
	"github.com/MelloB1989/karmax/fleet/internal/ledger"
)

// myName is who this fleetctl speaks as: the session's fleet name, else
// the operator.
func myName() string {
	if n := os.Getenv("FLEET_NAME"); n != "" { // set in every fleet container
		return n
	}
	if n := os.Getenv("FLEET_AGENT"); n != "" {
		return n
	}
	return ledger.Operator
}

// karmaxCmd is `karmax tools|call`: the operator's integrations that KARMAX
// holds, through fleetd, which alone holds KARMAX's token.
func karmaxCmd(args []string) error {
	const usage = `karmax tools [--json] | karmax call <tool> [k=v | k:=<json>]… [--json-args '{}'] --reason "<why>" [--timeout 30m] [--json]`
	if len(args) == 0 {
		return fmt.Errorf("usage: fleetctl %s", usage)
	}
	switch args[0] {
	case "tools":
		return getAndPrint(args[1:], "/v1/karmax/tools", func(raw json.RawMessage) error {
			var tools []daemon.GatewayTool
			if err := json.Unmarshal(raw, &tools); err != nil {
				return err
			}
			w := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
			fmt.Fprintln(w, "TOOL\tCLASS\tWHAT")
			for _, t := range tools {
				what, _, _ := strings.Cut(t.Description, "\n")
				if len(what) > 90 {
					what = what[:90] + "…"
				}
				fmt.Fprintf(w, "%s\t%s\t%s\n", t.Name, t.Class, what)
			}
			return w.Flush()
		})
	case "call":
		var reason, jsonArgs string
		var timeout time.Duration
		var asJSON bool
		_, pos, err := flags("karmax call", args[1:], func(fs *flag.FlagSet) {
			fs.StringVar(&reason, "reason", "", "")
			fs.StringVar(&jsonArgs, "json-args", "", "")
			fs.DurationVar(&timeout, "timeout", daemon.ApprovalTimeout, "")
			fs.BoolVar(&asJSON, "json", false, "")
		})
		if err != nil {
			return err
		}
		if err := need(pos, 1, usage); err != nil {
			return err
		}
		callArgs, err := toolArgs(jsonArgs, pos[1:])
		if err != nil {
			return err
		}
		c, err := newClient()
		if err != nil {
			return err
		}
		c.timeout = 20 * time.Minute // an approved write runs inside the wait's answer
		rep, err := karmaxCall(context.Background(), c, myName(), pos[0], callArgs, reason, timeout, os.Stderr, sleepCtx)
		if err != nil {
			return err
		}
		if asJSON {
			if err := printJSON(rep); err != nil {
				return err
			}
		} else if rep.Result != nil {
			var out any
			if json.Unmarshal(rep.Result.Output, &out) == nil {
				_ = printJSON(out)
			}
		}
		if rep.Result != nil && !rep.Result.OK {
			return fmt.Errorf("%s: %s", rep.Tool, rep.Result.Error)
		}
		return nil
	}
	return fmt.Errorf("usage: fleetctl %s", usage)
}

// toolArgs is a call's arguments: --json-args, then each k=v (a string) or
// k:=<json> (a number, a bool, a list…) over it. A phone number is a string
// unless it is given as JSON.
func toolArgs(jsonArgs string, kvs []string) (map[string]any, error) {
	out := map[string]any{}
	if strings.TrimSpace(jsonArgs) != "" {
		if err := json.Unmarshal([]byte(jsonArgs), &out); err != nil {
			return nil, fmt.Errorf("--json-args: want a JSON object: %w", err)
		}
	}
	for _, kv := range kvs {
		if k, v, ok := strings.Cut(kv, ":="); ok && k != "" && !strings.Contains(k, "=") {
			var x any
			if err := json.Unmarshal([]byte(v), &x); err != nil {
				return nil, fmt.Errorf("%s: %q is not JSON: %w", k, v, err)
			}
			out[k] = x
			continue
		}
		k, v, ok := strings.Cut(kv, "=")
		if !ok || k == "" {
			return nil, fmt.Errorf("argument %q: want k=v or k:=<json>", kv)
		}
		out[k] = v
	}
	return out, nil
}

// karmaxCall calls a tool through fleetd. A read comes back at once. A write
// comes back as a pending approval: this then waits on it (errOut says so)
// until the orchestrator decides or it expires. fleetd restarting mid-wait is
// a retry, 1 s doubling to 15 s; a refusal is an error that says why.
func karmaxCall(ctx context.Context, c *client, from, tool string, args map[string]any, reason string, timeout time.Duration, errOut io.Writer, sleep func(context.Context, time.Duration) error) (daemon.CallReply, error) {
	var rep daemon.CallReply
	body := map[string]any{"tool": tool, "args": args, "reason": reason, "timeout": timeout.String(), "from": from}
	if err := c.do("POST", "/v1/karmax/call", body, &rep); err != nil {
		return rep, err
	}
	if rep.Approval == nil || rep.Result != nil {
		return rep, nil
	}
	a := rep.Approval
	if rep.Notice != "" {
		fmt.Fprintln(errOut, "note:", rep.Notice)
	}
	fmt.Fprintf(errOut, "%s is a write: approval %d is with the orchestrator; waiting until %s (Ctrl-C leaves it to expire)\n",
		tool, a.ID, clock(a.Expires))
	const minBack, maxBack = time.Second, 15 * time.Second
	back := minBack
	var notice restartNotice
	path := fmt.Sprintf("/v1/approvals/%d?wait=5m&from=%s", a.ID, url.QueryEscape(from))
	for {
		var got daemon.CallReply
		err := c.do("GET", path, nil, &got)
		switch {
		case err == nil && got.Result != nil:
			return got, nil
		case err == nil: // still pending: wait again
			back, notice = minBack, restartNotice{}
			continue
		case !retryable(err):
			var se *statusError
			if errors.As(err, &se) && se.Code == 403 {
				return got, fmt.Errorf("not run: %w", err)
			}
			return got, err
		}
		notice.say(errOut)
		if err := sleep(ctx, back); err != nil {
			return got, err
		}
		back = min(back*2, maxBack)
	}
}

// approvalsCmd is `approvals`: the requests waiting for a decision, or all.
func approvalsCmd(args []string) error {
	var all, asJSON bool
	var n int
	if _, _, err := flags("approvals", args, func(fs *flag.FlagSet) {
		fs.BoolVar(&all, "all", false, "")
		fs.IntVar(&n, "n", 50, "")
		fs.BoolVar(&asJSON, "json", false, "")
	}); err != nil {
		return err
	}
	state := ledger.Pending
	if all {
		state = "all"
	}
	c, err := newClient()
	if err != nil {
		return err
	}
	var aps []ledger.Approval
	if err := c.do("GET", fmt.Sprintf("/v1/approvals?state=%s&n=%d", state, n), nil, &aps); err != nil {
		return err
	}
	if asJSON {
		return printJSON(aps)
	}
	if len(aps) == 0 {
		fmt.Println("no approvals " + map[bool]string{true: "on record", false: "pending"}[all])
		return nil
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(w, "ID\tAGENT\tTASK\tTOOL\tSTATE\tASKED\tUNTIL\tREASON")
	for _, a := range aps {
		fmt.Fprintf(w, "%d\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", a.ID, a.Agent, dash(a.Task), a.Tool, a.State, clock(a.At), clock(a.Expires), a.Reason)
	}
	return w.Flush()
}

// decideCmd is `approve <id>` and `deny <id> --reason …`.
func decideCmd(cmd string, args []string) error {
	by, reason := myName(), ""
	_, pos, err := flags(cmd, args, func(fs *flag.FlagSet) {
		fs.StringVar(&by, "by", by, "")
		fs.StringVar(&reason, "reason", "", "")
	})
	if err != nil {
		return err
	}
	what := cmd + " <id>"
	if cmd == "deny" {
		what += ` --reason "<why>"`
	}
	if err := need(pos, 1, what); err != nil {
		return err
	}
	id, err := strconv.ParseInt(pos[0], 10, 64)
	if err != nil {
		return fmt.Errorf("approval %q: want its number", pos[0])
	}
	c, err := newClient()
	if err != nil {
		return err
	}
	var a ledger.Approval
	if err := c.do("POST", fmt.Sprintf("/v1/approvals/%d/%s", id, cmd), map[string]string{"by": by, "reason": reason}, &a); err != nil {
		return err
	}
	fmt.Printf("%s %d: %s for %s (%s)\n", a.State, a.ID, a.Tool, a.Agent, dash(a.Task))
	return nil
}
