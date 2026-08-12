package main

import (
	"fmt"
	"time"

	"github.com/spf13/cobra"
)

// What inference actually costs.
//
// The gateway KARMAX ran on reported zero tokens for every call, so spend was
// unmeasurable and a 28k-token routing decision was indistinguishable from a 3k
// one. On metered inference that difference is the bill, so it gets a command.

// Published per-million-token rates, used only to turn token counts into a
// rough figure. Deliberately approximate: this is a tripwire, not an invoice,
// and the authority on spend is the provider's own billing.
var modelRates = map[string]struct{ in, out float64 }{
	"claude-sonnet-4.6": {3.00, 15.00},
	"claude-sonnet-4-6": {3.00, 15.00},
	"claude-haiku-4.5":  {1.00, 5.00},
	"claude-haiku-4-5":  {1.00, 5.00},
	"claude-opus-4.6":   {5.00, 25.00},
	"claude-opus-4-6":   {5.00, 25.00},
}

// rateFor matches a model name against the table, tolerating the provider
// prefixes and version suffixes Bedrock adds.
func rateFor(model string) (struct{ in, out float64 }, bool) {
	if r, ok := modelRates[model]; ok {
		return r, true
	}
	for name, r := range modelRates {
		if len(model) >= len(name) && contains(model, name) {
			return r, true
		}
	}
	return struct{ in, out float64 }{}, false
}

func contains(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}

func costCmd() *cobra.Command {
	var days int
	cmd := &cobra.Command{
		Use:   "cost",
		Short: "Show what the models have consumed",
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := openStore()
			if err != nil {
				return err
			}
			defer s.Close()

			since := time.Now().AddDate(0, 0, -days)
			totals, err := s.UsageSince(since)
			if err != nil {
				return err
			}
			if len(totals) == 0 {
				fmt.Printf("No usage recorded in the last %d day(s).\n", days)
				fmt.Println("If the daemon has been running, the provider is not reporting token counts.")
				return nil
			}

			fmt.Printf("Model usage, last %d day(s)\n\n", days)
			fmt.Printf("  %-26s %-9s %7s %12s %12s %10s\n", "MODEL", "INSTANCE", "CALLS", "INPUT", "OUTPUT", "EST. COST")
			var estTotal float64
			var anyUnpriced bool
			for _, t := range totals {
				est := ""
				if r, ok := rateFor(t.Model); ok {
					// Cache reads bill at roughly a tenth of fresh input.
					c := (float64(t.InputTokens)/1e6)*r.in +
						(float64(t.CacheRead)/1e6)*r.in*0.1 +
						(float64(t.OutputTokens)/1e6)*r.out
					estTotal += c
					est = fmt.Sprintf("$%.2f", c)
				} else {
					anyUnpriced = true
					est = "—"
				}
				fmt.Printf("  %-26s %-9s %7d %12d %12d %10s\n",
					truncModel(t.Model, 26), t.Kind, t.Calls, t.InputTokens, t.OutputTokens, est)
			}
			fmt.Printf("\n  estimated total: $%.2f", estTotal)
			if days > 0 {
				fmt.Printf("   (~$%.2f/month at this rate)", estTotal*30/float64(days))
			}
			fmt.Println()
			if anyUnpriced {
				fmt.Println("  (— means no published rate is known for that model)")
			}
			return nil
		},
	}
	cmd.Flags().IntVar(&days, "days", 7, "how many days back to total")
	return cmd
}

func truncModel(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}
