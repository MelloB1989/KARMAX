package main

import (
	"fmt"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/MelloB1989/karmax/internal/store"
	"github.com/spf13/cobra"
)

// The directory maps an external identity — a Slack user, a Jira reporter — to
// an org member.
//
// Nothing outside tests could write to it. That is what made per-user
// connectors unusable: the agent resolves who it is acting for from here, and
// with no rows it is acting for nobody, so every Google call was refused as
// "not on anyone's behalf" — which reads, from Slack, as the agent saying it
// cannot reach your mail.

func directoryCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "directory",
		Short: "Map Slack (and other) identities to org members",
		Long: "An agent acting for someone has to be able to name them: it is how a\n" +
			"per-user connector picks whose Google account to use, and how the audit\n" +
			"trail says who asked rather than only that the agent did it.",
	}
	cmd.AddCommand(directoryMapCmd(), directoryListCmd(), directoryUnmapCmd())
	return cmd
}

func directoryMapCmd() *cobra.Command {
	var name, org string
	cmd := &cobra.Command{
		Use:   "map <kind> <external-id> <member>",
		Short: "Point an external identity at an org member",
		Example: "  karmax directory map slack U0BM6564DK7 mellob --name 'Kartik Deshmukh'\n" +
			"  karmax directory map github kartik-dev mellob",
		Args: cobra.ExactArgs(3),
		RunE: func(_ *cobra.Command, args []string) error {
			kind, id, member := strings.TrimSpace(args[0]), strings.TrimSpace(args[1]), strings.TrimSpace(args[2])
			if kind == "" || id == "" || member == "" {
				return fmt.Errorf("kind, external id and member must all be non-empty")
			}
			s, err := openStore()
			if err != nil {
				return err
			}
			defer s.Close()

			// Best effort: a mesh identity is not required to map somebody, and
			// demanding one would block the common single-instance case.
			if org == "" {
				org, _ = orgKey()
			}
			if err := s.MapMember(store.Member{
				ExternalKind: kind, ExternalID: id, Member: member, Org: org, Name: name,
			}); err != nil {
				return err
			}
			fmt.Printf("%s %s now acts as %s.\n", kind, id, member)
			return nil
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "display name, for the audit trail")
	cmd.Flags().StringVar(&org, "org", "", "org key (default: this instance's)")
	return cmd
}

func directoryListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "Show every identity mapping",
		RunE: func(_ *cobra.Command, _ []string) error {
			s, err := openStore()
			if err != nil {
				return err
			}
			defer s.Close()

			members, err := s.Directory()
			if err != nil {
				return err
			}
			if len(members) == 0 {
				fmt.Println("Nothing mapped yet. Until something is, the agent cannot act as")
				fmt.Println("anyone, and per-user connectors (Google) will refuse every call.")
				fmt.Println("\nAdd one with: karmax directory map slack <user-id> <member>")
				return nil
			}
			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "KIND\tEXTERNAL ID\tMEMBER\tNAME")
			for _, m := range members {
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", m.ExternalKind, m.ExternalID, m.Member, orDash(m.Name, "-"))
			}
			return w.Flush()
		},
	}
}

func directoryUnmapCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "unmap <kind> <external-id>",
		Short: "Remove an identity mapping",
		Args:  cobra.ExactArgs(2),
		RunE: func(_ *cobra.Command, args []string) error {
			s, err := openStore()
			if err != nil {
				return err
			}
			defer s.Close()
			if err := s.UnmapMember(args[0], args[1]); err != nil {
				return err
			}
			fmt.Printf("%s %s no longer maps to anyone.\n", args[0], args[1])
			return nil
		},
	}
}
