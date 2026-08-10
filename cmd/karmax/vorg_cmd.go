package main

import (
	"fmt"
	"os"
	"time"

	"github.com/MelloB1989/karmax/internal/mesh"
	"github.com/MelloB1989/karmax/internal/store"
	"github.com/MelloB1989/karmax/internal/vorg"
	"github.com/spf13/cobra"
)

const vorgExample = `name: research-pod
shared_memory: pod-shared
budget: 300000
certificate_ttl: 168h

roles:
  - id: researcher
    name: Ada
    charter: finds and reads sources, and says what it could not find
    department: research
    tools: [github.issues]
    # instance: <their mesh key — karmax mesh id on their machine>

  - id: writer
    name: Bea
    charter: turns findings into a brief, and never invents a citation
    budget: 100000

wiring:
  - from: researcher
    to: writer
    verbs: [ask]
`

func vorgCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "vorg",
		Aliases: []string{"virtual-org"},
		Short:   "Instantiate an organisation from a spec",
		Long: "A virtual organisation is roles, wiring, shared memory and a budget, written\n" +
			"down once and applied. It is federation, not multi-tenancy: each role is a\n" +
			"separate KARMAX instance that the org reaches with a scoped certificate.",
	}
	cmd.AddCommand(vorgPlanCmd(), vorgApplyCmd(), vorgExampleCmd())
	return cmd
}

func loadSpec(path string) (*vorg.Spec, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	s, err := vorg.Parse(data)
	if err != nil {
		return nil, fmt.Errorf("%s:%w", path, err)
	}
	return s, nil
}

func buildPlan(path string) (*vorg.Plan, *mesh.Node, *store.Store, error) {
	s, err := loadSpec(path)
	if err != nil {
		return nil, nil, nil, err
	}
	n, db, err := openMesh()
	if err != nil {
		return nil, nil, nil, err
	}
	orgName := envOr("KARMAX_MESH_ORG_NAME", s.Name)
	return vorg.Build(s, n.Identity().ID(), orgName), n, db, nil
}

func vorgPlanCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "plan <spec.yaml>",
		Short: "Show what applying a spec would do, without doing it",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			p, _, db, err := buildPlan(args[0])
			if err != nil {
				return err
			}
			defer db.Close()
			fmt.Print(p.Report())
			fmt.Println("\nNothing above has happened. Run `karmax vorg apply` to carry it out.")
			return nil
		},
	}
}

// applier wires a plan to this instance's mesh identity and store.
type applier struct {
	node  *mesh.Node
	store *store.Store
	// issued keeps the certificates so apply can print them for delivery.
	issued map[string]*mesh.Certificate
}

func (a *applier) Hire(m store.OrgMember) error { return a.store.SaveOrgMember(m) }

func (a *applier) Issue(subject string, scopes []string, ttl time.Duration) error {
	cert, err := a.node.IssueFor(subject, scopes, ttl)
	if err != nil {
		return err
	}
	a.issued[subject] = cert
	return nil
}

func (a *applier) Grant(g store.Grant) error { return a.store.SaveGrant(g) }

func vorgApplyCmd() *cobra.Command {
	var yes bool
	cmd := &cobra.Command{
		Use:   "apply <spec.yaml>",
		Short: "Carry out a spec: record roles, issue certificates, grant capabilities",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			p, n, db, err := buildPlan(args[0])
			if err != nil {
				return err
			}
			defer db.Close()

			fmt.Print(p.Report())
			if len(p.Actions) == 0 {
				return nil
			}
			if !yes {
				// Certificates go to other people's machines and cannot be
				// recalled, so this asks once rather than assuming.
				fmt.Print("\nApply this? [y/N] ")
				var answer string
				fmt.Scanln(&answer)
				if answer != "y" && answer != "Y" {
					fmt.Println("Nothing applied.")
					return nil
				}
			}

			a := &applier{node: n, store: db, issued: map[string]*mesh.Certificate{}}
			if err := p.Apply(a); err != nil {
				return err
			}

			fmt.Printf("\nApplied. %d certificate(s) issued.\n", len(a.issued))
			fmt.Println("\nDeliver each with:")
			for subject := range a.issued {
				fmt.Printf("  karmax mesh org send --to <their endpoint> --subject %s\n", short(subject))
			}
			fmt.Println("\nEach member's operator must set KARMAX_MESH_ORG_KEY to this org's key")
			fmt.Println("first, or their instance will refuse the certificate — which is the point.")
			return nil
		},
	}
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "do not ask for confirmation")
	return cmd
}

func vorgExampleCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "example",
		Short: "Print a spec to start from",
		RunE: func(_ *cobra.Command, _ []string) error {
			fmt.Print(vorgExample)
			return nil
		},
	}
}
