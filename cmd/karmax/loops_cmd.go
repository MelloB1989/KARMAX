package main

import (
	"fmt"
	"net/url"
	"os"
	"text/tabwriter"

	"github.com/MelloB1989/karmax/internal/loopinstall"
	"github.com/MelloB1989/karmax/internal/recipes"
	"github.com/MelloB1989/karmax/internal/wasmloop"
	"github.com/spf13/cobra"
)

// The operator's view of everything automating something, across all three
// tiers, plus the on/off switch. Installing lives in `karmax wloop`, and
// writing one in `karmax recipe`.

func newLoopsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "loops",
		Short: "Everything that runs on your behalf: recipes, signed loops, and their switches",
		Long: "Three tiers, in order of how much they can do:\n\n" +
			"  recipes       one YAML file, no install       — karmax recipe\n" +
			"  signed loops  sandboxed WASM, capability-gated — karmax wloop\n" +
			"  compiled-in   first-party, full authority      — shipped with KARMAX\n\n" +
			"This command lists them and turns them on and off.",
	}
	cmd.AddCommand(loopsListCmd(), loopsRunCmd(), loopsDisableCmd(), loopsEnableCmd(),
		loopsBrowseCmd(), loopsInstallCmd())
	return cmd
}

func loopsListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "Everything automating something, and which tier it is in",
		RunE: func(_ *cobra.Command, _ []string) error {
			disabled := loopinstall.LoadDisabledLoops()
			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "NAME\tTIER\tTRIGGER\tSTATE")

			var any bool
			for _, l := range recipes.Valid(recipes.LoadAll(recipes.Dir())) {
				any = true
				fmt.Fprintf(w, "%s\trecipe\t%s\t%s\n", l.Name, triggerOf(l), state(disabled[l.Name]))
			}

			in := &wasmloop.Installer{Dir: wasmloop.Dir()}
			entries, err := in.Installed()
			if err == nil {
				for _, e := range entries {
					any = true
					fmt.Fprintf(w, "%s\tsigned (%s)\t%s\t%s\n",
						e.Name, e.Tier, "see manifest", state(disabled[e.Name] || !e.Enabled))
				}
			}
			w.Flush()

			if !any {
				fmt.Println("Nothing installed yet.")
				fmt.Println("  karmax recipe example   — start a recipe")
				fmt.Println("  karmax wloop --help     — install a signed loop")
			}
			return nil
		},
	}
}

func state(disabled bool) string {
	if disabled {
		return stMuted.Render("disabled")
	}
	return stGreen.Render("on")
}

func loopsRunCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "run <name>",
		Short: "Run one now, without waiting for its schedule",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			out, err := apiPOST("/api/loops/" + url.PathEscape(args[0]) + "/run")
			if err != nil {
				return fmt.Errorf("could not reach the daemon — is KARMAX running? (%w)", err)
			}
			fmt.Printf("ran: %s\n", asStr(out["ran"]))
			return nil
		},
	}
}

func loopsDisableCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "disable <name>",
		Short: "Stop one running, without uninstalling it",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if err := loopinstall.SetLoopDisabled(args[0], true); err != nil {
				return err
			}
			fmt.Printf("%s is disabled. Restart KARMAX for it to take effect.\n", args[0])
			return nil
		},
	}
}

func loopsEnableCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "enable <name>",
		Short: "Turn one back on",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if err := loopinstall.SetLoopDisabled(args[0], false); err != nil {
				return err
			}
			fmt.Printf("%s is enabled. Restart KARMAX for it to take effect.\n", args[0])
			return nil
		},
	}
}
