package main

import (
	"fmt"
	"sort"
	"time"

	"github.com/MelloB1989/karmax/internal/config"
	"github.com/MelloB1989/karmax/internal/cost"
	"github.com/MelloB1989/karmax/internal/store"
	"github.com/spf13/cobra"
)

// What inference actually costs.
//
// The gateway KARMAX ran on reported zero tokens for every call, so spend was
// unmeasurable and a 28k-token routing decision was indistinguishable from a 3k
// one. On metered inference that difference is the bill, so it gets a command.
//
// The rates now live in internal/cost, because the agent needs the same answer
// this command gives and a second copy of the table would drift from it.

func costCmd() *cobra.Command {
	var days int
	var daily bool
	cmd := &cobra.Command{
		Use:   "cost",
		Short: "Show what the models have consumed, against the budget",
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
			fmt.Printf("  %-26s %-9s %7s %12s %12s %12s %10s\n",
				"MODEL", "INSTANCE", "CALLS", "INPUT", "CACHED", "OUTPUT", "EST. COST")
			var estTotal float64
			var anyUnpriced bool
			for _, t := range totals {
				est := "—"
				if c, ok := cost.Estimate(cost.Usage{
					Model: t.Model, InputTokens: t.InputTokens, OutputTokens: t.OutputTokens,
					CacheRead: t.CacheRead, CacheWrite: t.CacheWrite,
				}); ok {
					estTotal += c
					est = fmt.Sprintf("$%.2f", c)
				} else {
					anyUnpriced = true
				}
				fmt.Printf("  %-26s %-9s %7d %12d %12d %12d %10s\n",
					truncModel(t.Model, 26), t.Kind, t.Calls, t.InputTokens, t.CacheRead, t.OutputTokens, est)
			}

			if daily {
				if err := printDailyCost(s, since); err != nil {
					return err
				}
			}

			budget := cost.Budget{Spent: estTotal, Days: cost.WindowDays(since)}
			if cfg, cerr := config.Load(findConfig()); cerr == nil {
				budget.MonthlyUSD = cfg.Karmax.BudgetUSDPerMonth
			}
			fmt.Printf("\n  estimated total: $%.2f   (~$%.2f/month at this rate)\n",
				estTotal, budget.Projected())
			if budget.MonthlyUSD > 0 {
				fmt.Printf("  budget:          $%.2f/month — %s", budget.MonthlyUSD, budget.Status())
				if h := budget.Headroom(); h < 0 {
					fmt.Printf(" by $%.2f", -h)
				} else {
					fmt.Printf(", $%.2f/month to spare", h)
				}
				fmt.Println()
			} else {
				fmt.Println("  budget:          none set (karmax.budget_usd_per_month)")
			}
			if anyUnpriced {
				fmt.Println("  (— means no published rate is known for that model)")
			}
			return nil
		},
	}
	cmd.Flags().IntVar(&days, "days", 7, "how many days back to total")
	cmd.Flags().BoolVar(&daily, "daily", false, "break the total down by day")
	return cmd
}

// printDailyCost shows spend per day, so a step change is visible as one.
func printDailyCost(s *store.Store, since time.Time) error {
	rows, err := s.UsageByDay(since)
	if err != nil {
		return err
	}
	byDay := map[string]float64{}
	callsByDay := map[string]int{}
	for _, r := range rows {
		c, _ := cost.Estimate(cost.Usage{
			Model: r.Model, InputTokens: r.InputTokens, OutputTokens: r.OutputTokens,
			CacheRead: r.CacheRead, CacheWrite: r.CacheWrite,
		})
		byDay[r.Day] += c
		callsByDay[r.Day] += r.Calls
	}
	days := make([]string, 0, len(byDay))
	for d := range byDay {
		days = append(days, d)
	}
	sort.Strings(days)

	fmt.Printf("\n  %-12s %7s %10s\n", "DAY", "CALLS", "EST. COST")
	for _, d := range days {
		fmt.Printf("  %-12s %7d %10s\n", d, callsByDay[d], fmt.Sprintf("$%.2f", byDay[d]))
	}
	return nil
}

func truncModel(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}
