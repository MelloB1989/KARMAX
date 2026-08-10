package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/MelloB1989/karmax/internal/broker"
	"github.com/MelloB1989/karmax/internal/config"
	"github.com/MelloB1989/karmax/internal/store"
	"github.com/spf13/cobra"
	"go.uber.org/zap"
)

// The operator's view of the broker. A permission model nobody can inspect is a
// permission model nobody trusts.

func capsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "caps",
		Aliases: []string{"capabilities"},
		Short:   "See and change what loops, peers and connectors may do",
	}
	cmd.AddCommand(capsListCmd(), capsGrantCmd(), capsRevokeCmd(), capsMeterCmd())
	return cmd
}

func openStore() (*store.Store, error) {
	cfg, err := config.Load(findConfig())
	if err != nil {
		return nil, err
	}
	return store.New(filepath.Join(cfg.Karmax.DataDir, "db", "karmax.db"), zap.NewNop())
}

func capsListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list [subject]",
		Short: "List capability grants",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			s, err := openStore()
			if err != nil {
				return err
			}
			defer s.Close()

			var grants []store.Grant
			if len(args) == 1 {
				grants, err = s.Grants(args[0])
			} else {
				grants, err = s.AllGrants()
			}
			if err != nil {
				return err
			}
			if len(grants) == 0 {
				fmt.Println("No grants recorded. Loops with no grants run ungated (they are compiled into the daemon);")
				fmt.Println("granting a loop anything is what starts enforcing the rest.")
				return nil
			}
			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "SUBJECT\tCAPABILITY\tVALUE\tEXPIRES")
			for _, g := range grants {
				expires := "never"
				if g.ExpiresAt != nil {
					expires = g.ExpiresAt.Format(time.RFC3339)
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", g.Subject, g.Capability, g.Value, expires)
			}
			return w.Flush()
		},
	}
}

func capsGrantCmd() *cobra.Command {
	var ttl time.Duration
	cmd := &cobra.Command{
		Use:   "grant <subject> <capability> <value>",
		Short: "Grant a capability",
		Long: "Subjects are loop:<name>, peer:<key> or connector:<id>.\n" +
			"Capabilities are tool, http, memory, channel and spend.\n\n" +
			"  karmax caps grant loop:tech-news http api.github.com\n" +
			"  karmax caps grant loop:tech-news tool 'memory.*'\n" +
			"  karmax caps grant loop:tech-news spend 50000\n\n" +
			"NOTE: granting a loop its first capability switches it from ungated to\n" +
			"enforced, so grant everything it needs in one go.",
		Args: cobra.ExactArgs(3),
		RunE: func(_ *cobra.Command, args []string) error {
			s, err := openStore()
			if err != nil {
				return err
			}
			defer s.Close()

			g := store.Grant{Subject: args[0], Capability: args[1], Value: args[2]}
			if ttl > 0 {
				at := time.Now().Add(ttl)
				g.ExpiresAt = &at
			}
			if err := s.SaveGrant(g); err != nil {
				return err
			}
			existing, _ := s.Grants(g.Subject)
			fmt.Printf("Granted. %s now holds %d capabilities:\n", g.Subject, len(existing))
			b := broker.New(s, zap.NewNop())
			lines, _ := b.Describe(g.Subject)
			for _, l := range lines {
				fmt.Println("  - " + l)
			}
			if strings.HasPrefix(g.Subject, "loop:") {
				fmt.Println("\nRestart KARMAX for this to take effect on the loop.")
			}
			return nil
		},
	}
	cmd.Flags().DurationVar(&ttl, "ttl", 0, "expire the grant after this long (default: never)")
	return cmd
}

func capsRevokeCmd() *cobra.Command {
	var all bool
	cmd := &cobra.Command{
		Use:   "revoke <subject> [capability] [value]",
		Short: "Withdraw a capability",
		Args:  cobra.RangeArgs(1, 3),
		RunE: func(_ *cobra.Command, args []string) error {
			s, err := openStore()
			if err != nil {
				return err
			}
			defer s.Close()

			if all || len(args) == 1 {
				n, err := s.RevokeSubject(args[0])
				if err != nil {
					return err
				}
				fmt.Printf("Revoked %d grants from %s.\n", n, args[0])
				if strings.HasPrefix(args[0], "loop:") && n > 0 {
					fmt.Println("With no grants left it returns to running ungated on the next restart.")
				}
				return nil
			}
			if len(args) != 3 {
				return fmt.Errorf("revoking one capability needs a value; use --all to revoke everything")
			}
			if err := s.RevokeGrant(args[0], args[1], args[2]); err != nil {
				return err
			}
			fmt.Println("Revoked.")
			return nil
		},
	}
	cmd.Flags().BoolVar(&all, "all", false, "revoke every capability this subject holds")
	return cmd
}

func capsMeterCmd() *cobra.Command {
	var days int
	cmd := &cobra.Command{
		Use:   "meter",
		Short: "What each subject has used, and what it was refused",
		RunE: func(_ *cobra.Command, args []string) error {
			s, err := openStore()
			if err != nil {
				return err
			}
			defer s.Close()

			readings, err := s.Meter(time.Now().AddDate(0, 0, -days), 500)
			if err != nil {
				return err
			}
			if len(readings) == 0 {
				fmt.Println("Nothing metered yet.")
				return nil
			}
			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "DAY\tSUBJECT\tCAPABILITY\tALLOWED\tREFUSED\tUNITS")
			for _, r := range readings {
				fmt.Fprintf(w, "%s\t%s\t%s\t%d\t%d\t%d\n",
					r.Day, r.Subject, r.Capability, r.Allowed, r.Refused, r.Units)
			}
			return w.Flush()
		},
	}
	cmd.Flags().IntVar(&days, "days", 7, "how far back to report")
	return cmd
}
