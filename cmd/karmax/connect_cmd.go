package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/MelloB1989/karmax/internal/browser"
	"github.com/MelloB1989/karmax/internal/setupagent"
	"github.com/spf13/cobra"
)

func connectCmd() *cobra.Command {
	var account string
	cmd := &cobra.Command{
		Use:   "connect [service]",
		Short: "Let the assistant connect a service for you",
		Long: "Some services are twenty minutes of clicking through a console. This does that\n" +
			"part, in the browser you are already signed into, and tells you when it needs\n" +
			"you to press something.\n\n" +
			"Run it with no arguments to see what it can connect.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if len(args) == 0 {
				return listConnectable()
			}
			recipe, ok := setupagent.Lookup(args[0])
			if !ok {
				return fmt.Errorf("nothing here knows how to connect %q — try `karmax connect` for the list", args[0])
			}
			return setupagent.Run(context.Background(), recipe, setupagent.Options{
				DataDir: dataDir(),
				Browser: browser.Shared(dataDir()),
				Account: account,
			}, printProgress)
		},
	}
	cmd.Flags().StringVar(&account, "account", "", "which account to connect, when you have more than one")
	return cmd
}

func listConnectable() error {
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "SERVICE\tNEEDS\t")
	for _, id := range setupagent.IDs() {
		r, _ := setupagent.Lookup(id)
		needs := strings.Join(r.Needs, ", ")
		if needs == "" {
			needs = "—"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\n", r.ID, needs, r.Lede)
	}
	return w.Flush()
}

// printProgress writes the run to a terminal, marking the moments that are
// somebody's turn so they are not missed in a scroll.
func printProgress(p setupagent.Progress) {
	switch p.Kind {
	case "doing":
		fmt.Printf("   · %s\n", p.Text)
	case "needs-you":
		fmt.Printf("\n>> %s\n\n", p.Text)
	case "failed":
		fmt.Printf("\n!! %s\n", p.Text)
	case "done":
		if strings.TrimSpace(p.Text) != "" {
			fmt.Printf("\n%s\n", p.Text)
		}
	default:
		fmt.Printf("%s\n", p.Text)
	}
}
