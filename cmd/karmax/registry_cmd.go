package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/MelloB1989/karmax/internal/loopregistry"
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
//
// The fetch, digest-check, and per-tier install itself live in
// internal/loopregistry now, not here — the app's HTTP API needs the exact
// same answers and cannot drive a terminal prompt to get them. What stays
// here is the part that IS a terminal prompt: printing a preview and asking.

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
			installed := loopregistry.InstalledNames()

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

// installRecipe previews a recipe and, once approved, writes it via
// loopregistry.WriteRecipe. Parsing happens here (not just in the write path)
// because the preview needs the parsed Recipe before anything is written.
func installRecipe(e wasmloop.RegistryEntry, data []byte, yes bool) error {
	r, err := loopregistry.ParseRecipeArtifact(e.Name, data)
	if err != nil {
		return err
	}

	fmt.Printf("%s %s — %s\n", e.Name, e.Version, firstLine(e.Description))
	fmt.Printf("  kind      recipe (one YAML file, interpreted — not compiled code)\n")
	if trigger := loopregistry.RecipeTrigger(r); trigger != "" {
		fmt.Printf("  runs      %s\n", trigger)
	}
	fmt.Println("\nIt will:")
	for _, line := range recipes.Describe(r) {
		fmt.Println("  - " + line)
	}
	path := recipesPath(e.Name)
	if _, err := os.Stat(path); err == nil {
		fmt.Printf("\nThis REPLACES the %s recipe already on this machine.\n", e.Name)
	}

	if !yes && !confirm("\nInstall it? [y/N] ") {
		fmt.Println("Nothing installed.")
		return nil
	}
	wrote, _, err := loopregistry.WriteRecipe(e.Name, data)
	if err != nil {
		return err
	}
	fmt.Printf("\nWrote %s. KARMAX picks it up without a restart.\n", wrote)
	return nil
}

func recipesPath(name string) string { return filepath.Join(recipes.Dir(), name+".yaml") }

// installWorkflow previews the artifact (openStore/trustFromEnv wire this
// instance's own trust configuration, same as `karmax wloop install`) and,
// once approved, installs it via loopregistry.InstallWorkflow — the same
// digest and signature verification the API's install endpoint runs.
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
	// Inspect once, unrelaxed, purely to show the preview and drive the
	// interactive confirmation below — the tier the OPERATOR's own trust
	// config reaches, not the lenient one loopregistry.InstallWorkflow uses
	// internally to decide against allowUntrusted.
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
		untrusted = true
	} else if !yes && !confirm("\nInstall it? [y/N] ") {
		fmt.Println("Nothing installed.")
		return nil
	}
	if _, _, err := loopregistry.InstallWorkflow(in, data, untrusted); err != nil {
		if errors.Is(err, loopregistry.ErrUntrusted) {
			// Can't happen: confirmUnreviewed above already turned untrusted on
			// for exactly this case. Guarded anyway rather than assumed.
			fmt.Println("Nothing installed.")
			return nil
		}
		return err
	}
	fmt.Printf("\nInstalled %s %s. Restart KARMAX to run it.\n", e.Name, e.Version)
	return nil
}

func confirm(prompt string) bool {
	fmt.Print(prompt)
	var answer string
	fmt.Scanln(&answer)
	return answer == "y" || answer == "Y"
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
