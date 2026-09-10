package main

import (
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/MelloB1989/karmax/internal/config"
	"github.com/MelloB1989/karmax/internal/fsscope"
	"github.com/spf13/cobra"
)

func dataDir() string {
	if cfg, err := config.Load(findConfig()); err == nil {
		return cfg.Karmax.DataDir
	}
	return ""
}

func accessCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "access",
		Short: "What the assistant may touch on this machine",
		Long: "KARMAX runs a coding harness with real file and shell tools on your laptop.\n" +
			"This is where you say which folders it works in, and which places it can never\n" +
			"reach. Restricting a place is a hard block — it stops `cat` through the shell,\n" +
			"not only the file tools.",
		RunE: func(*cobra.Command, []string) error { return showAccess() },
	}
	cmd.AddCommand(accessShowCmd(), accessAllowCmd(), accessRevokeCmd(),
		accessRestrictCmd(), accessUnrestrictCmd(), accessOffCmd())
	return cmd
}

func showAccess() error {
	dir := dataDir()
	p := fsscope.Load(dir)
	if !p.Enforced {
		fmt.Println("No limits set: the assistant can reach anything you can.")
		fmt.Println("Set one with `karmax access restrict <folder>` or `karmax access allow <folder>`.")
		return nil
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	if len(p.Grants) > 0 {
		fmt.Fprintln(w, "WORKS IN\t")
		for _, g := range p.Grants {
			how := "read only"
			if g.Write {
				how = "read and write"
			}
			fmt.Fprintf(w, "  %s\t%s\n", g.Path, how)
		}
		fmt.Fprintln(w, "\t")
	}
	fmt.Fprintln(w, "NEVER REACHABLE\t")
	for _, d := range p.Deny {
		fmt.Fprintf(w, "  %s\tyours\n", d)
	}
	for _, d := range fsscope.StandardDenies(dir) {
		fmt.Fprintf(w, "  %s\tstandard\n", d)
	}
	if err := w.Flush(); err != nil {
		return err
	}
	fmt.Printf("\nStored in %s\n", fsscope.Path(dir))
	return nil
}

func accessShowCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "show",
		Short: "What it may touch right now",
		RunE:  func(*cobra.Command, []string) error { return showAccess() },
	}
}

func accessAllowCmd() *cobra.Command {
	var write bool
	cmd := &cobra.Command{
		Use:   "allow <folder>",
		Short: "Point it at a folder to work in",
		Long: "Where the assistant works, handed to the harness as its working set.\n\n" +
			"This is an instruction, not a fence: allowing one folder does not put the\n" +
			"others out of reach. Use `restrict` for that. What allow does enforce is the\n" +
			"other half — without --write, edits there are blocked.",
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			return update(func(p *fsscope.Policy) error { return p.Allow(args[0], write) })
		},
	}
	cmd.Flags().BoolVar(&write, "write", false, "let it change files there, not only read them")
	return cmd
}

func accessRevokeCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "revoke <folder>",
		Short: "Stop pointing it at a folder",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			return update(func(p *fsscope.Policy) error { return p.Revoke(args[0]) })
		},
	}
}

func accessRestrictCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "restrict <path>",
		Aliases: []string{"deny", "forbid"},
		Short:   "Put a place out of reach for good",
		Args:    cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			return update(func(p *fsscope.Policy) error { return p.Forbid(args[0]) })
		},
	}
}

func accessUnrestrictCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "unrestrict <path>",
		Short: "Take back one of your own restrictions",
		Long: "Only your own. The standard list — SSH keys, cloud credentials, the browser\n" +
			"profile, KARMAX's own store — is not stored anywhere and cannot be removed.",
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			return update(func(p *fsscope.Policy) error { return p.Unforbid(args[0]) })
		},
	}
}

func accessOffCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "off",
		Short: "Drop every limit (the assistant can reach anything you can)",
		RunE: func(*cobra.Command, []string) error {
			if err := fsscope.Save(dataDir(), fsscope.Policy{}); err != nil {
				return err
			}
			fmt.Println("Limits off. The assistant can reach anything you can.")
			return nil
		},
	}
}

func update(change func(*fsscope.Policy) error) error {
	dir := dataDir()
	p := fsscope.Load(dir)
	if err := change(&p); err != nil {
		return err
	}
	if err := fsscope.Save(dir, p); err != nil {
		return err
	}
	return showAccess()
}
