package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/MelloB1989/karmax/fleet/internal/daemon"
	"github.com/MelloB1989/karmax/fleet/internal/fleetapi"
	"github.com/MelloB1989/karmax/fleet/internal/ledger"
)

const quotaWaitUse = "usage: fleetctl quota wait [<agent|account>…] [--all] [--timeout 12h] [--json]"

// quotaCmd is `fleetctl quota`: each account's windows, then the agents its
// quota holds back from new tasks and when that lifts. `quota wait` waits
// for that.
func quotaCmd(args []string) error {
	if len(args) > 0 && args[0] == "wait" {
		return quotaWaitCmd(args[1:])
	}
	return getAndPrint(args, "/v1/quota", func(raw json.RawMessage) error {
		var q map[string]ledger.Quota
		if err := json.Unmarshal(raw, &q); err != nil {
			return err
		}
		accts := make([]string, 0, len(q))
		for a := range q {
			accts = append(accts, a)
		}
		slices.Sort(accts)
		now := time.Now()
		w := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
		fmt.Fprintln(w, "ACCOUNT\t5H%\tRESETS\t7D%\tRESETS\tWEEK ROOM\tAS OF\tSOURCE")
		for _, acct := range accts {
			x := q[acct].Aged(now)
			fmt.Fprintf(w, "%s\t%.0f\t%s\t%.0f\t%s\t%.0f%%/day\t%s\t%s\n", acct, x.FiveHour, clock(x.FiveResets), x.SevenDay,
				clock(x.SevenResets), fleetapi.WeekRoom(x.SevenDay, x.SevenResets, now), clock(x.At), x.Source)
		}
		if err := w.Flush(); err != nil {
			return err
		}
		fmt.Printf("WEEK ROOM is the weekly limit left a day until its reset (a fresh week: %.0f%%/day); new tasks go\nto the agents with the most of it first, so no week runs out days before its reset.\n", fleetapi.WeekEven)
		c, err := newClient()
		if err != nil {
			return err
		}
		rows, err := statusRows(c)
		if err != nil {
			return nil // the windows are printed; the holds are a bonus
		}
		var held []agentHold
		for _, h := range agentHolds(rows) {
			if h.Held() {
				held = append(held, h)
			}
		}
		if len(held) == 0 {
			fmt.Println("\nNo agent is held back by its quota.")
			return nil
		}
		fmt.Println()
		w = tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
		fmt.Fprintln(w, "HELD BACK\tACCOUNT\tWHY\tLIFTS")
		for _, h := range held {
			lifts := "not known yet"
			if !h.Until.IsZero() {
				lifts = daemon.When(h.Until, now)
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", h.Agent, h.Account, strings.Join(h.Why, ", "), lifts)
		}
		if err := w.Flush(); err != nil {
			return err
		}
		fmt.Println("fleetctl quota wait [<agent>…] waits until a hold lifts.")
		return nil
	})
}

// agentHold is an agent's quota hold: its slots share its account and its
// rate limit, so one hold for all of them (each slot's, merged).
type agentHold struct {
	Agent   string `json:"agent"`
	Account string `json:"account"`
	daemon.Hold
}

// agentHolds is every agent's hold, in roster order.
func agentHolds(rows []daemon.Row) []agentHold {
	var out []agentHold
	at := map[string]int{}
	unknown := map[string]bool{}
	for _, r := range rows {
		i, ok := at[r.SlotOf]
		if !ok {
			i = len(out)
			at[r.SlotOf] = i
			out = append(out, agentHold{Agent: r.SlotOf, Account: r.Account, Hold: daemon.Hold{Why: []string{}}})
		}
		h := r.Hold()
		if !h.Held() {
			continue
		}
		a := &out[i]
		for _, why := range h.Why {
			if !slices.Contains(a.Why, why) {
				a.Why = append(a.Why, why)
			}
		}
		if h.Until.IsZero() {
			unknown[r.SlotOf] = true
		} else if h.Until.After(a.Until) {
			a.Until = h.Until
		}
	}
	for i := range out {
		if unknown[out[i].Agent] {
			out[i].Until = time.Time{}
		}
	}
	return out
}

func statusRows(c *client) ([]daemon.Row, error) {
	var out struct {
		Agents []daemon.Row `json:"agents"`
	}
	err := c.do("GET", "/v1/status", nil, &out)
	return out.Agents, err
}

// quotaWaitResult is what `quota wait` found. Reason: "free" (a hold it
// waited for lifted), "none" (nothing to wait for), "timeout".
type quotaWaitResult struct {
	Reason string      `json:"reason"`
	Free   []string    `json:"free"`
	Held   []agentHold `json:"held"`
}

func quotaWaitCmd(args []string) error {
	var all, asJSON bool
	var timeout time.Duration
	_, names, err := flags("quota wait", args, func(fs *flag.FlagSet) {
		fs.BoolVar(&all, "all", false, "")
		fs.DurationVar(&timeout, "timeout", 12*time.Hour, "")
		fs.BoolVar(&asJSON, "json", false, "")
	})
	if err != nil {
		return err
	}
	if timeout <= 0 {
		return errors.New(quotaWaitUse)
	}
	c, err := newClient()
	if err != nil {
		return err
	}
	r, err := waitQuota(context.Background(), c, names, all, timeout, 15*time.Second, os.Stderr, sleepCtx)
	if err != nil {
		return err
	}
	if asJSON {
		_ = printJSON(r)
	} else {
		printQuotaWait(os.Stdout, r, names, timeout, time.Now())
	}
	if r.Reason == "timeout" {
		return exitCode{4}
	}
	return nil
}

// holdLines is one line per held agent: "  agent-02  5h 85%, lifts at 03:50 (in 4h22m)".
func holdLines(holds []agentHold, now time.Time) string {
	var b strings.Builder
	tw := tabwriter.NewWriter(&b, 0, 2, 2, ' ', 0)
	for _, h := range holds {
		fmt.Fprintf(tw, "  %s\t%s\n", h.Agent, h.String(now))
	}
	_ = tw.Flush()
	return b.String()
}

func printQuotaWait(w io.Writer, r quotaWaitResult, names []string, timeout time.Duration, now time.Time) {
	switch r.Reason {
	case "none":
		if len(names) == 0 {
			fmt.Fprintln(w, "no agent is held back by its quota")
		} else {
			fmt.Fprintln(w, strings.Join(r.Free, ", ")+": not held back by quota")
		}
	case "free":
		fmt.Fprintln(w, "free of its quota hold now: "+strings.Join(r.Free, ", "))
		if len(r.Held) > 0 {
			fmt.Fprint(w, "still held:\n"+holdLines(r.Held, now))
		}
	case "timeout":
		fmt.Fprintf(w, "still held after %s:\n%s", timeout, holdLines(r.Held, now))
	}
}

// waitQuota waits until a hold lifts: on the agents named (an agent, one
// of its slots, or an account names its agents), else on every agent held
// back now. It returns when one of them is free, or, with all, when every
// one is. It reads the roster every poll; fleetd restarting is a line on
// errOut and the next poll.
func waitQuota(ctx context.Context, c *client, names []string, all bool, timeout, poll time.Duration, errOut io.Writer, sleep func(context.Context, time.Duration) error) (quotaWaitResult, error) {
	deadline := time.Now().Add(timeout)
	var notice restartNotice
	read := func() ([]agentHold, error) {
		for {
			c.timeout = 30 * time.Second
			rows, err := statusRows(c)
			if err == nil {
				return agentHolds(rows), nil
			}
			if !retryable(err) {
				return nil, err
			}
			left := time.Until(deadline)
			if left <= 0 {
				return nil, nil
			}
			notice.say(errOut)
			if err := sleep(ctx, min(poll, left)); err != nil {
				return nil, err
			}
		}
	}
	holds, err := read()
	if err != nil || holds == nil {
		return quotaWaitResult{Reason: "timeout"}, err
	}
	var targets []string
	if len(names) == 0 {
		for _, h := range holds {
			if h.Held() {
				targets = append(targets, h.Agent)
			}
		}
	} else {
		for _, n := range names {
			found := false
			for _, h := range holds {
				if n == h.Agent || n == h.Account || strings.HasPrefix(n, h.Agent+":") {
					found = true
					if !slices.Contains(targets, h.Agent) {
						targets = append(targets, h.Agent)
					}
				}
			}
			if !found {
				return quotaWaitResult{}, fmt.Errorf("no agent or account %s", n)
			}
		}
	}
	split := func(holds []agentHold) (free []string, held []agentHold) {
		for _, h := range holds {
			if !slices.Contains(targets, h.Agent) {
				continue
			}
			if h.Held() {
				held = append(held, h)
			} else {
				free = append(free, h.Agent)
			}
		}
		return free, held
	}
	free, held := split(holds)
	if len(held) == 0 {
		return quotaWaitResult{Reason: "none", Free: free, Held: []agentHold{}}, nil
	}
	waiting := map[string]bool{}
	for _, h := range held {
		waiting[h.Agent] = true
	}
	fmt.Fprint(errOut, "waiting for a quota hold to lift:\n"+holdLines(held, time.Now()))
	last := held
	for {
		left := time.Until(deadline)
		if left <= 0 {
			return quotaWaitResult{Reason: "timeout", Free: []string{}, Held: last}, nil
		}
		if err := sleep(ctx, min(poll, left)); err != nil {
			return quotaWaitResult{}, err
		}
		holds, err := read()
		if err != nil {
			return quotaWaitResult{}, err
		}
		if holds == nil {
			continue // fleetd did not answer before the deadline
		}
		free, held := split(holds)
		var freed []string
		for _, a := range free {
			if waiting[a] {
				freed = append(freed, a)
			}
		}
		last = held
		if len(freed) > 0 && (!all || len(held) == 0) {
			return quotaWaitResult{Reason: "free", Free: freed, Held: nonNil(held)}, nil
		}
	}
}

func nonNil(h []agentHold) []agentHold {
	if h == nil {
		return []agentHold{}
	}
	return h
}
