package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/MelloB1989/karmax/fleet/internal/fleetapi"
)

const accessUse = `access [--json] | access admin <device> | access lend <device> <agent>… | access unlend <device> <agent>… | access revoke <device>`

// lendCaveat is said after every lend: a guest at the keyboard of an agent
// can read the Claude login it runs on.
const lendCaveat = `Whoever types into an agent can read its Claude login. Lend agents whose subscription you are
happy to share, or the guest's own.`

// accessCmd is `access` (GET /v1/access) and its changes (POST /v1/access):
// which devices may use the fleet, as admin or as a guest with lent sandbox
// agents, and which have asked.
func accessCmd(args []string) error {
	var asJSON bool
	_, pos, err := flags("access", args, func(fs *flag.FlagSet) { fs.BoolVar(&asJSON, "json", false, "") })
	if err != nil {
		return err
	}
	c, err := newClient()
	if err != nil {
		return err
	}
	var l fleetapi.AccessList
	if len(pos) == 0 {
		if err := c.do("GET", fleetapi.PathAccess, nil, &l); err != nil {
			return err
		}
	} else {
		ch, err := accessChange(pos)
		if err != nil {
			return err
		}
		if err := c.do("POST", fleetapi.PathAccess, ch, &l); err != nil {
			return err
		}
	}
	if asJSON {
		return printJSON(l)
	}
	printAccess(os.Stdout, l, time.Now())
	if len(pos) > 0 && pos[0] == fleetapi.AccessLend {
		fmt.Println()
		fmt.Println(lendCaveat)
	}
	return nil
}

// accessChange reads `<op> <device> [<agent>…]`.
func accessChange(pos []string) (fleetapi.AccessChange, error) {
	use := errors.New("usage: fleetctl " + accessUse)
	if len(pos) < 2 {
		return fleetapi.AccessChange{}, use
	}
	ch := fleetapi.AccessChange{Op: pos[0], Device: pos[1], Agents: pos[2:]}
	switch ch.Op {
	case fleetapi.AccessAdmin, fleetapi.AccessRevoke:
		if len(ch.Agents) > 0 {
			return ch, use
		}
	case fleetapi.AccessLend, fleetapi.AccessUnlend:
		if len(ch.Agents) == 0 {
			return ch, use
		}
	default:
		return ch, use
	}
	return ch, nil
}

// printAccess writes the tagged devices, then the knocks.
func printAccess(w io.Writer, l fleetapi.AccessList, now time.Time) {
	tw := tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "DEVICE\tLOGIN\tROLE\tAGENTS\tSEEN")
	for _, d := range l.Devices {
		agents := strings.Join(d.Agents, ",")
		switch {
		case d.Role == fleetapi.RoleAdmin:
			agents = "all"
		case agents == "":
			agents = "-"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", d.Name, d.Login, d.Role, agents, seenAgo(d.LastSeen, now))
	}
	_ = tw.Flush()
	if len(l.Devices) == 0 {
		fmt.Fprintln(w, "no tagged devices: only fleetd's own machine, with the full token, may use the fleet")
	}
	if len(l.Knocks) == 0 {
		return
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Asked, not tagged (fleetctl access lend <device> <agent>…, or admin):")
	tw = tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "DEVICE\tLOGIN\tASKED\tLAST")
	for _, k := range l.Knocks {
		fmt.Fprintf(tw, "%s\t%s\t%d×\t%s\n", k.Name, k.Login, k.Count, seenAgo(k.Last, now))
	}
	_ = tw.Flush()
}

// seenAgo is how long before now t was, "-" for never.
func seenAgo(t, now time.Time) string {
	if t.IsZero() {
		return "-"
	}
	d := now.Sub(t).Round(time.Minute)
	switch {
	case d < time.Minute:
		return "now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	}
	return fmt.Sprintf("%dd ago", int(d.Hours()/24))
}
