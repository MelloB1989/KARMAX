package main

import (
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/MelloB1989/karmax/internal/reflex"
	"github.com/spf13/cobra"
)

// What System One has been deciding.
//
// Thresholds are the whole risk surface of screening: set too high and the
// brain is woken for noise, too low and work is lost. Neither can be judged in
// the abstract — only against traffic that actually arrived — so the verdicts
// are recorded and this is where they are read back.

func reflexCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "reflex",
		Short: "Inspect the fast screening layer in front of the brain",
	}
	cmd.AddCommand(reflexStatsCmd(), reflexRecentCmd())
	return cmd
}

func reflexStatsCmd() *cobra.Command {
	var days int
	cmd := &cobra.Command{
		Use:   "stats",
		Short: "Show what screening has decided, and what it saved",
		RunE: func(_ *cobra.Command, _ []string) error {
			s, err := openStore()
			if err != nil {
				return err
			}
			defer s.Close()

			since := time.Now().AddDate(0, 0, -days)
			counts, err := s.ReflexActionCounts(since)
			if err != nil {
				return err
			}
			if len(counts) == 0 {
				fmt.Printf("No screening recorded in the last %d day(s).\n", days)
				fmt.Println("Either reflex is off, or it has no TypeSafe key — check `reflex.enabled` and TYPESAFE_API_KEY.")
				return nil
			}

			total := 0
			for _, n := range counts {
				total += n
			}
			fmt.Printf("Screening, last %d day(s)\n\n", days)
			fmt.Printf("  %-12s %8s %8s\n", "ACTION", "COUNT", "SHARE")

			actions := make([]string, 0, len(counts))
			for a := range counts {
				actions = append(actions, a)
			}
			sort.Strings(actions)
			for _, a := range actions {
				fmt.Printf("  %-12s %8d %7.1f%%\n", a, counts[a], 100*float64(counts[a])/float64(total))
			}

			// The saving is the events that never reached a model at all.
			spared := counts[string(reflex.ActionDrop)] + counts[string(reflex.ActionRemember)]
			fmt.Printf("\n  %d of %d events (%.1f%%) never reached the brain.\n",
				spared, total, 100*float64(spared)/float64(total))
			if counts[string(reflex.ActionDelegate)] > 0 {
				fmt.Printf("  %d went straight to a harness without a routing turn.\n",
					counts[string(reflex.ActionDelegate)])
			}
			return nil
		},
	}
	cmd.Flags().IntVar(&days, "days", 7, "how many days back to total")
	return cmd
}

func reflexRecentCmd() *cobra.Command {
	var limit int
	var action string
	cmd := &cobra.Command{
		Use:   "recent",
		Short: "Show recent verdicts with the numbers behind them",
		RunE: func(_ *cobra.Command, _ []string) error {
			s, err := openStore()
			if err != nil {
				return err
			}
			defer s.Close()

			verdicts, err := s.RecentReflexVerdicts(limit)
			if err != nil {
				return err
			}
			if len(verdicts) == 0 {
				fmt.Println("Nothing screened yet.")
				return nil
			}
			for _, v := range verdicts {
				if action != "" && v.Action != action {
					continue
				}
				var parsed reflex.Verdict
				_ = json.Unmarshal([]byte(v.Verdict), &parsed)
				fmt.Printf("%s  %-10s %-22s conf %.2f  urg %.2f  risk %.2f  %s\n",
					v.CreatedAt.Format("Jan 02 15:04"), v.Action, v.EventKind,
					parsed.Confidence, parsed.Urgency, parsed.Risk, parsed.Reason)
			}
			return nil
		},
	}
	cmd.Flags().IntVar(&limit, "limit", 40, "how many verdicts to show")
	cmd.Flags().StringVar(&action, "action", "", "show only this action (drop, remember, handle, delegate, escalate)")
	return cmd
}
