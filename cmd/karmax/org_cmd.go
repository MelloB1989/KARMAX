package main

import (
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/MelloB1989/karmax/internal/mesh"
	"github.com/MelloB1989/karmax/internal/store"
	"github.com/spf13/cobra"
)

// The org chart, from the org instance's side.
//
// An organisation here is federation, not multi-tenancy: one instance per
// person plus one for the org, joined by certificates. So "hiring" is recording
// somebody in the chart and issuing them a certificate — not creating a row in
// a tenant table that the org can then read.

func orgChartCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "org-chart",
		Short: "Departments, members, and what each may do",
	}
	cmd.AddCommand(orgHireCmd(), orgListCmd(), orgRemoveCmd(), orgGrantCmd())
	return cmd
}

func orgKey() (string, error) {
	n, _, err := openMesh()
	if err != nil {
		return "", err
	}
	return n.Identity().ID(), nil
}

func orgHireCmd() *cobra.Command {
	var dept, role, name string
	cmd := &cobra.Command{
		Use:   "hire <member-key>",
		Short: "Record an instance in the org chart",
		Long: "The member key is what `karmax mesh id` prints on their machine.\n" +
			"Hiring records them and derives their department's memory namespace;\n" +
			"issue them a certificate with `karmax org-chart grant` to give them access.",
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			member := args[0]
			if _, err := mesh.DecodeKey(member); err != nil {
				return fmt.Errorf("that is not an instance key: %w", err)
			}
			org, err := orgKey()
			if err != nil {
				return err
			}
			s, err := openStore()
			if err != nil {
				return err
			}
			defer s.Close()

			orgName := envOr("KARMAX_MESH_ORG_NAME", "org")
			m := store.OrgMember{
				Org: org, Member: member, Name: name, Department: dept, Role: role,
				Namespace: store.OrgNamespace(orgName, dept),
			}
			if err := s.SaveOrgMember(m); err != nil {
				return err
			}
			fmt.Printf("Recorded %s in %s.\n", displayName(m), orDash(dept, "no department"))
			fmt.Printf("Their department's memory namespace is %s.\n", m.Namespace)
			fmt.Printf("\nNext: karmax org-chart grant %s --scope ask --scope 'memory:%s'\n", member, m.Namespace)
			return nil
		},
	}
	cmd.Flags().StringVar(&dept, "department", "", "which department they are in")
	cmd.Flags().StringVar(&role, "role", "", "what they do")
	cmd.Flags().StringVar(&name, "name", "", "display name")
	return cmd
}

func orgListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "Show the org chart",
		RunE: func(_ *cobra.Command, _ []string) error {
			org, err := orgKey()
			if err != nil {
				return err
			}
			s, err := openStore()
			if err != nil {
				return err
			}
			defer s.Close()

			members, err := s.OrgMembers(org)
			if err != nil {
				return err
			}
			if len(members) == 0 {
				fmt.Println("Nobody in the org chart yet. Add someone with `karmax org-chart hire <key>`.")
				return nil
			}
			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "DEPARTMENT\tNAME\tROLE\tNAMESPACE\tKEY")
			for _, m := range members {
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n",
					orDash(m.Department, "-"), displayName(m), orDash(m.Role, "-"),
					m.Namespace, short(m.Member))
			}
			return w.Flush()
		},
	}
}

func orgRemoveCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "remove <member-key>",
		Short: "Take someone out of the org chart",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			org, err := orgKey()
			if err != nil {
				return err
			}
			s, err := openStore()
			if err != nil {
				return err
			}
			defer s.Close()
			if err := s.RemoveOrgMember(org, args[0]); err != nil {
				return err
			}
			fmt.Println("Removed from the org chart.")
			fmt.Println("Their existing certificate still works until it expires — stop reissuing it,")
			fmt.Println("or have them run `karmax mesh block` on this org's key to cut it now.")
			return nil
		},
	}
}

func orgGrantCmd() *cobra.Command {
	var scopes []string
	var ttl time.Duration
	cmd := &cobra.Command{
		Use:   "grant <member-key>",
		Short: "Issue a certificate carrying transport verbs and capabilities",
		Long: "Scopes are either transport verbs (message, ask, broadcast) or capabilities\n" +
			"written as class:value — tool:github.issues, memory:org-vector-eng, spend:100000.\n\n" +
			"The member's instance decides whether to honour the capabilities; this only\n" +
			"states what the org is asking for.",
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			n, s, err := openMesh()
			if err != nil {
				return err
			}
			defer s.Close()

			if len(scopes) == 0 {
				return fmt.Errorf("no scopes — a certificate granting nothing is not worth issuing")
			}
			if ttl <= 0 {
				ttl = 30 * 24 * time.Hour
			}
			cert, err := n.IssueFor(args[0], scopes, ttl)
			if err != nil {
				return err
			}

			var transport, caps []string
			for _, sc := range scopes {
				if strings.Contains(sc, ":") {
					caps = append(caps, sc)
				} else {
					transport = append(transport, sc)
				}
			}
			fmt.Printf("Certificate issued, valid until %s.\n\n",
				time.Unix(cert.Expires, 0).Format(time.RFC1123))
			fmt.Printf("  may contact this instance by: %s\n", orDash(strings.Join(transport, ", "), "nothing"))
			fmt.Printf("  asks for capabilities:        %s\n", orDash(strings.Join(caps, ", "), "none"))
			fmt.Println("\nDeliver it with `karmax mesh org send`. The member's operator must have")
			fmt.Println("set KARMAX_MESH_ORG_KEY to this org's key, or it will be refused.")
			return nil
		},
	}
	cmd.Flags().StringArrayVar(&scopes, "scope", nil,
		"a transport verb or class:value capability (repeatable)")
	cmd.Flags().DurationVar(&ttl, "ttl", 0, "how long it is valid (default 30 days)")
	return cmd
}

func displayName(m store.OrgMember) string {
	if strings.TrimSpace(m.Name) != "" {
		return m.Name
	}
	return short(m.Member)
}

func orDash(s, fallback string) string {
	if strings.TrimSpace(s) == "" {
		return fallback
	}
	return s
}

func short(id string) string {
	if len(id) > 14 {
		return id[:14] + "…"
	}
	return id
}
