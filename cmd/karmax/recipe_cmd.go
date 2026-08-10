package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"

	"github.com/MelloB1989/karmax/internal/recipes"
	"github.com/MelloB1989/karmax/pkg/loopkit"
	"github.com/spf13/cobra"
)

const exampleRecipe = `name: pr-review-nudge
on:
  event: github.event

steps:
  - recall: "context about {{ .repo }}"
    as: ctx

  - ask: |
      A review was requested on "{{ .title }}" in {{ .repo }}.
      Given this context: {{ .ctx }}
      Is this urgent enough to interrupt me? Answer yes or no, then why.
    as: verdict

  - when: "{{ .verdict }}"
    notify:
      title: "Review requested: {{ .title }}"
      body: "{{ .verdict }}"
    else:
      - remind:
          title: "Review {{ .title }}"
          notes: "{{ .url }}"
`

func recipeCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "recipe",
		Aliases: []string{"recipes"},
		Short:   "Automations written as one YAML file — no install, no rebuild",
		Long: "Recipes live in " + recipes.Dir() + ".\n" +
			"Drop a file there and it is live within seconds. That is the whole install step.",
	}
	cmd.AddCommand(recipeListCmd(), recipeCheckCmd(), recipeTestCmd(),
		recipeEjectCmd(), recipeExampleCmd())
	return cmd
}

// loadOne finds a recipe by its name, its filename, or a path.
//
// The name is what `recipe list` prints and what the file's own 'name:' says,
// and the two need not match — so accepting only the filename would mean the
// name shown by one command is rejected by the next.
func loadOne(nameOrPath string) (*recipes.Recipe, error) {
	if strings.ContainsAny(nameOrPath, "/\\") ||
		strings.HasSuffix(nameOrPath, ".yaml") || strings.HasSuffix(nameOrPath, ".yml") {
		data, err := os.ReadFile(nameOrPath)
		if err != nil {
			return nil, err
		}
		return recipes.Parse(nameOrPath, data)
	}

	var known []string
	for _, l := range recipes.LoadAll(recipes.Dir()) {
		base := strings.TrimSuffix(filepath.Base(l.Path), filepath.Ext(l.Path))
		if base == nameOrPath {
			return l.Recipe, l.Err
		}
		if l.Err == nil && l.Recipe != nil {
			if l.Recipe.Name == nameOrPath {
				return l.Recipe, nil
			}
			known = append(known, l.Recipe.Name)
		}
	}
	if len(known) == 0 {
		return nil, fmt.Errorf("no recipes in %s", recipes.Dir())
	}
	return nil, fmt.Errorf("no recipe %q — there is %s", nameOrPath, strings.Join(known, ", "))
}

func recipeListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "Show every recipe and whether it is valid",
		RunE: func(_ *cobra.Command, _ []string) error {
			loaded := recipes.LoadAll(recipes.Dir())
			if len(loaded) == 0 {
				fmt.Printf("No recipes in %s.\nStart one with `karmax recipe example > %s/my-first.yaml`.\n",
					recipes.Dir(), recipes.Dir())
				return nil
			}
			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "NAME\tTRIGGER\tSTEPS\tSTATUS")
			broken := 0
			for _, l := range loaded {
				if l.Err != nil {
					broken++
					fmt.Fprintf(w, "%s\t-\t-\tBROKEN\n", filepath.Base(l.Path))
					continue
				}
				status := "ok"
				if !l.Recipe.IsEnabled() {
					status = "disabled"
				}
				fmt.Fprintf(w, "%s\t%s\t%d\t%s\n",
					l.Recipe.Name, triggerOf(l.Recipe), len(l.Recipe.Steps), status)
			}
			w.Flush()
			if broken > 0 {
				fmt.Printf("\n%d file(s) have problems — run `karmax recipe check <name>` to see them.\n", broken)
			}
			return nil
		},
	}
}

func triggerOf(r *recipes.Recipe) string {
	switch {
	case r.On.Schedule != "":
		return "schedule " + r.On.Schedule
	case r.On.Event != "":
		return "event " + r.On.Event
	case r.On.Webhook != "":
		return "webhook " + r.On.Webhook
	default:
		return "manual"
	}
}

func recipeCheckCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "check [name]",
		Short: "Validate a recipe, or all of them",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if len(args) == 1 {
				r, err := loadOne(args[0])
				if err != nil {
					return err
				}
				fmt.Printf("%s is valid: %d steps, %s.\n", r.Name, len(r.Steps), triggerOf(r))
				return nil
			}
			var bad int
			for _, l := range recipes.LoadAll(recipes.Dir()) {
				if l.Err != nil {
					bad++
					fmt.Println(l.Err.Error())
					fmt.Println()
				}
			}
			if bad > 0 {
				return fmt.Errorf("%d recipe(s) have problems", bad)
			}
			fmt.Println("All recipes are valid.")
			return nil
		},
	}
}

func recipeTestCmd() *cobra.Command {
	var payload []string
	cmd := &cobra.Command{
		Use:   "test <name>",
		Short: "Show what a recipe would do, without doing any of it",
		Long: "Every side effect is described instead of performed and the clock is fake,\n" +
			"so a 72h wait resolves instantly. Nothing is sent, stored or spent.",
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			r, err := loadOne(args[0])
			if err != nil {
				return err
			}
			p := map[string]any{}
			for _, kv := range payload {
				k, v, ok := strings.Cut(kv, "=")
				if !ok {
					return fmt.Errorf("--payload expects key=value, got %q", kv)
				}
				p[k] = v
			}

			kind := loopkit.TriggerManual
			switch {
			case r.On.Schedule != "":
				kind = loopkit.TriggerSchedule
			case r.On.Event != "":
				kind = loopkit.TriggerEvent
			case r.On.Webhook != "":
				kind = loopkit.TriggerWebhook
			}

			dry := recipes.NewDryRun(loopkit.Trigger{Kind: kind, Payload: p})
			fmt.Printf("If %q ran now, it would:\n\n", r.Name)
			if err := recipes.Run(context.Background(), r, dry); err != nil {
				fmt.Print(dry.Report())
				return fmt.Errorf("\nand then fail: %w", err)
			}
			fmt.Print(dry.Report())
			fmt.Println("\nNothing above actually happened.")
			return nil
		},
	}
	cmd.Flags().StringArrayVar(&payload, "payload", nil,
		"key=value to stand in for trigger data (repeatable)")
	return cmd
}

func recipeEjectCmd() *cobra.Command {
	var out string
	cmd := &cobra.Command{
		Use:   "eject <name>",
		Short: "Turn a recipe into the equivalent Go loop",
		Long: "For when a recipe outgrows YAML. The generated loop does the same things in the\n" +
			"same order with the same step names, so a run part-way through resumes correctly.",
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			r, err := loadOne(args[0])
			if err != nil {
				return err
			}
			code := recipes.Eject(r, "")
			if out == "" {
				fmt.Print(code)
				return nil
			}
			if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
				return err
			}
			if err := os.WriteFile(out, []byte(code), 0o644); err != nil {
				return err
			}
			fmt.Printf("Wrote %s.\nDelete the recipe once the loop is installed, or they will both run.\n", out)
			return nil
		},
	}
	cmd.Flags().StringVarP(&out, "out", "o", "", "write to a file instead of stdout")
	return cmd
}

func recipeExampleCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "example",
		Short: "Print a recipe to start from",
		RunE: func(_ *cobra.Command, _ []string) error {
			fmt.Print(exampleRecipe)
			return nil
		},
	}
}
