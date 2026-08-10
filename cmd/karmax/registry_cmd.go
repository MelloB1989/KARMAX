package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/MelloB1989/karmax/internal/recipes"
	"github.com/MelloB1989/karmax/internal/wasmloop"
	"github.com/spf13/cobra"
)

// Installing from the registry.
//
// One command for both tiers. An operator asking "give me the news digest"
// should not have to know whether that is a YAML recipe or a signed WASM
// module — the difference belongs in what they are asked to approve, not in
// which command they had to guess.

func loopsBrowseCmd() *cobra.Command {
	var all bool
	return &cobra.Command{
		Use:   "browse",
		Short: "What the registry has",
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, cancel := context.WithTimeout(cmd.Context(), 60*time.Second)
			defer cancel()

			c := wasmloop.NewClient()
			idx, err := c.Index(ctx)
			if err != nil {
				return err
			}
			installed := installedNames()

			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "NAME\tKIND\tVERSION\tSTATE\tDESCRIPTION")
			shown := 0
			for _, e := range idx.Entries {
				state := ""
				if installed[e.Name] {
					state = "installed"
				} else if e.ShipWithKARMAX {
					state = "ships with karmax"
				}
				if !all && installed[e.Name] {
					continue
				}
				shown++
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n",
					e.Name, e.Kind, e.Version, state, firstLine(e.Description))
			}
			if err := w.Flush(); err != nil {
				return err
			}
			if shown == 0 {
				fmt.Println("Everything in the registry is already installed.")
			}
			fmt.Printf("\n%s\nInstall one with `karmax loops install <name>`.\n", c.BaseURL)
			return nil
		},
	}
}

func loopsInstallCmd() *cobra.Command {
	var yes, untrusted bool
	cmd := &cobra.Command{
		Use:   "install <name>",
		Short: "Install a recipe or workflow from the registry",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := context.WithTimeout(cmd.Context(), 5*time.Minute)
			defer cancel()

			c := wasmloop.NewClient()
			idx, err := c.Index(ctx)
			if err != nil {
				return err
			}
			e, ok := idx.Find(args[0])
			if !ok {
				return fmt.Errorf("the registry has nothing called %q — `karmax loops browse` lists what it does have", args[0])
			}
			data, err := c.Fetch(ctx, e)
			if err != nil {
				return err
			}

			switch e.Kind {
			case wasmloop.KindRecipe:
				return installRecipe(e, data, yes)
			case wasmloop.KindWorkflow:
				return installWorkflow(e, data, yes, untrusted)
			}
			return fmt.Errorf("%s is a %q, which this KARMAX does not know how to install", e.Name, e.Kind)
		},
	}
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "do not ask for confirmation")
	cmd.Flags().BoolVar(&untrusted, "untrusted", false,
		"accept a workflow no registry you trust has countersigned")
	return cmd
}

// installRecipe writes a recipe to the recipes directory.
//
// No signature and no sandbox, because a recipe is not code: it is data KARMAX
// interprets with its own tools, under the same Broker every other caller
// passes. What it can do is the union of the verbs it uses, and those are
// listed below before anything is written.
func installRecipe(e wasmloop.RegistryEntry, data []byte, yes bool) error {
	dir := recipes.Dir()
	path := filepath.Join(dir, e.Name+".yaml")

	r, err := recipes.Parse(path, data)
	if err != nil {
		return fmt.Errorf("the registry's copy of %s is not a valid recipe: %w", e.Name, err)
	}

	fmt.Printf("%s %s — %s\n", e.Name, e.Version, firstLine(e.Description))
	fmt.Printf("  kind      recipe (one YAML file, interpreted — not compiled code)\n")
	if trigger := recipeTrigger(r); trigger != "" {
		fmt.Printf("  runs      %s\n", trigger)
	}
	fmt.Println("\nIt will:")
	for _, line := range recipes.Describe(r) {
		fmt.Println("  - " + line)
	}
	if _, err := os.Stat(path); err == nil {
		fmt.Printf("\nThis REPLACES the %s recipe already on this machine.\n", e.Name)
	}

	if !yes && !confirm("\nInstall it? [y/N] ") {
		fmt.Println("Nothing installed.")
		return nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return err
	}
	fmt.Printf("\nWrote %s. KARMAX picks it up without a restart.\n", path)
	return nil
}

// installWorkflow runs the artifact through the same install path a local file
// takes, so a registry download gets no shortcut past the checks.
func installWorkflow(e wasmloop.RegistryEntry, data []byte, yes, untrusted bool) error {
	s, err := openStore()
	if err != nil {
		return err
	}
	defer s.Close()

	in := &wasmloop.Installer{
		Dir: wasmloop.Dir(), Broker: brokerStore{s},
		Trust: trustFromEnv(false, untrusted), Actor: os.Getenv("USER"),
	}
	p, err := in.Inspect(data)
	if err != nil {
		return err
	}
	showPreview(p)

	if p.Verdict.Tier != wasmloop.TierRegistry {
		if err := confirmUnreviewed(p); err != nil {
			fmt.Println("Nothing installed.")
			return nil
		}
	} else if !yes && !confirm("\nInstall it? [y/N] ") {
		fmt.Println("Nothing installed.")
		return nil
	}
	if _, err := in.Install(data); err != nil {
		return err
	}
	fmt.Printf("\nInstalled %s %s. Restart KARMAX to run it.\n", e.Name, e.Version)
	return nil
}

// installedNames is what is already here, across both tiers.
func installedNames() map[string]bool {
	out := map[string]bool{}
	in := &wasmloop.Installer{Dir: wasmloop.Dir()}
	if entries, err := in.Installed(); err == nil {
		for _, e := range entries {
			out[e.Name] = true
		}
	}
	for _, l := range recipes.LoadAll(recipes.Dir()) {
		if l.Recipe != nil {
			out[l.Recipe.Name] = true
		}
	}
	return out
}

func confirm(prompt string) bool {
	fmt.Print(prompt)
	var answer string
	fmt.Scanln(&answer)
	return answer == "y" || answer == "Y"
}

func recipeTrigger(r *recipes.Recipe) string {
	switch {
	case r.On.Schedule != "":
		return r.On.Schedule
	case r.On.Event != "":
		return "on " + r.On.Event
	case r.On.Webhook != "":
		return "webhook " + r.On.Webhook
	case r.On.Manual:
		return "only when you run it"
	}
	return ""
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = strings.TrimSpace(s[:i])
	}
	if len(s) > 72 {
		s = s[:72] + "…"
	}
	return s
}
