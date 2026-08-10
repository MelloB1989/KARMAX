package main

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/MelloB1989/karmax/internal/broker"
	"github.com/MelloB1989/karmax/internal/connectors"
	githubconn "github.com/MelloB1989/karmax/internal/connectors/github"
	"github.com/MelloB1989/karmax/internal/store"
	"github.com/MelloB1989/karmax/pkg/connectorkit"
	"github.com/spf13/cobra"
	"go.uber.org/zap"
)

// registered mirrors the runtime's registry. Connectors are compiled in, so
// both sides list the same set; keeping the CLI able to configure one without
// the daemon running is worth the duplication.
func registered() []connectorkit.Connector {
	return []connectorkit.Connector{githubconn.New()}
}

func connectorsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "connectors",
		Aliases: []string{"connector"},
		Short:   "Configure integrations (GitHub, and more as they land)",
	}
	cmd.AddCommand(connectorsListCmd(), connectorsSetupCmd(), connectorsEnableCmd(),
		connectorsDisableCmd(), connectorsCheckCmd())
	return cmd
}

func connectorsListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "Show every connector and whether it is set up",
		RunE: func(_ *cobra.Command, _ []string) error {
			s, err := openStore()
			if err != nil {
				return err
			}
			defer s.Close()

			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "ID\tNAME\tSTATUS\tDESCRIPTION")
			for _, c := range registered() {
				m := c.Manifest()
				status := "not configured"
				if rec, err := s.Credential(m.ID); err == nil {
					status = "configured, disabled"
					if rec.Enabled {
						status = "enabled"
					}
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", m.ID, m.Name, status, m.Description)
			}
			return w.Flush()
		},
	}
}

func connectorsSetupCmd() *cobra.Command {
	var set []string
	cmd := &cobra.Command{
		Use:   "setup <id>",
		Short: "Supply a connector's configuration",
		Long: "Values are given as --set key=value, repeated.\n\n" +
			"  karmax connectors setup github --set token=ghp_… --set default_repo=MelloB1989/karmax\n\n" +
			"Run without --set to see what the connector needs.",
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			id := args[0]
			var conn connectorkit.Connector
			for _, c := range registered() {
				if c.Manifest().ID == id {
					conn = c
				}
			}
			if conn == nil {
				return fmt.Errorf("no connector %q — try `karmax connectors list`", id)
			}
			m := conn.Manifest()

			if len(set) == 0 {
				fmt.Printf("%s needs:\n\n", m.Name)
				for _, f := range m.Config {
					req := ""
					if f.Required {
						req = "  (required)"
					}
					secret := ""
					if f.Secret {
						secret = "  [secret]"
					}
					fmt.Printf("  %-16s %s%s%s\n", f.Key, f.Description, req, secret)
				}
				fmt.Printf("\nIt will be granted exactly:\n")
				for _, c := range m.Capabilities {
					fmt.Printf("  - %s\n", c)
				}
				return nil
			}

			s, err := openStore()
			if err != nil {
				return err
			}
			defer s.Close()

			cfg := map[string]string{}
			if rec, err := s.Credential(id); err == nil {
				cfg = rec.Config
			}
			if cfg == nil {
				cfg = map[string]string{}
			}
			for _, kv := range set {
				k, v, ok := strings.Cut(kv, "=")
				if !ok {
					return fmt.Errorf("--set expects key=value, got %q", kv)
				}
				cfg[strings.TrimSpace(k)] = v
			}

			var missing []string
			for _, f := range m.Config {
				if f.Required && strings.TrimSpace(cfg[f.Key]) == "" {
					missing = append(missing, f.Key)
				}
			}
			sort.Strings(missing)
			if len(missing) > 0 {
				return fmt.Errorf("still missing: %s", strings.Join(missing, ", "))
			}

			if err := s.SaveCredential(store.Credential{Connector: id, Config: cfg}); err != nil {
				return err
			}
			fmt.Printf("Saved. Enable it with `karmax connectors enable %s`.\n", id)
			return nil
		},
	}
	cmd.Flags().StringArrayVar(&set, "set", nil, "key=value (repeatable)")
	return cmd
}

func connectorsEnableCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "enable <id>",
		Short: "Turn a connector on and grant it what its manifest declared",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id := args[0]
			s, err := openStore()
			if err != nil {
				return err
			}
			defer s.Close()

			if _, err := s.Credential(id); err != nil {
				return fmt.Errorf("%s is not configured yet — run `karmax connectors setup %s`", id, id)
			}

			host := connectors.NewHost(s, nil, broker.New(s, zap.NewNop()), zap.NewNop())
			for _, c := range registered() {
				host.Register(c)
			}
			if err := host.GrantFromManifest(id); err != nil {
				return err
			}
			if err := s.SetConnectorEnabled(id, true); err != nil {
				return err
			}

			fmt.Printf("%s is enabled and may:\n", id)
			b := broker.New(s, zap.NewNop())
			lines, _ := b.Describe(broker.ConnectorSubject(id))
			for _, l := range lines {
				fmt.Println("  - " + l)
			}
			fmt.Println("\nRestart KARMAX to mount its tools and webhooks.")
			return nil
		},
	}
}

func connectorsDisableCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "disable <id>",
		Short: "Turn a connector off, keeping its configuration",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			s, err := openStore()
			if err != nil {
				return err
			}
			defer s.Close()
			if err := s.SetConnectorEnabled(args[0], false); err != nil {
				return err
			}
			// Its capabilities go with it: a disabled connector that still holds
			// grants is a permission nobody remembers granting.
			if err := broker.New(s, zap.NewNop()).RevokeAll(broker.ConnectorSubject(args[0])); err != nil {
				return err
			}
			fmt.Printf("%s is disabled and its capabilities are revoked. Restart KARMAX to unmount it.\n", args[0])
			return nil
		},
	}
}

func connectorsCheckCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "check <id>",
		Short: "Verify a connector's credentials actually work",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			id := args[0]
			s, err := openStore()
			if err != nil {
				return err
			}
			defer s.Close()

			rec, err := s.Credential(id)
			if err != nil {
				return fmt.Errorf("%s is not configured", id)
			}
			var conn connectorkit.Connector
			for _, c := range registered() {
				if c.Manifest().ID == id {
					conn = c
				}
			}
			if conn == nil {
				return fmt.Errorf("no connector %q", id)
			}

			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			cr := connectorkit.Credentials{Config: rec.Config, AccessToken: rec.AccessToken}
			if err := conn.Health(ctx, cr); err != nil {
				return fmt.Errorf("%s is not working: %w", id, err)
			}
			fmt.Printf("%s is working.\n", id)
			return nil
		},
	}
}
