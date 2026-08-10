package main

import (
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
)

// The controls for the one thing KARMAX does that strangers can see.
//
// Posting is automatic and nobody reads a draft before it goes out, so the two
// commands that matter are "stop" and "what did you say". Both work against a
// running daemon: the switch is read from the store at post time rather than at
// startup, so `karmax social off` takes effect on the next post.

func socialCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "social",
		Short: "Control and inspect automatic posting to X and LinkedIn",
		Long: "KARMAX can write posts about your work and publish them without asking.\n" +
			"These commands stop it, start it again, and show what it has said.\n\n" +
			"Every post is checked before it goes out: it may not name anyone from\n" +
			"your contacts or memory, contain money, phone numbers, emails or\n" +
			"anything shaped like a credential. Refused drafts are logged too.",
		RunE: func(cmd *cobra.Command, args []string) error { return socialStatus() },
	}
	cmd.AddCommand(socialOffCmd(), socialOnCmd(), socialLogCmd())
	return cmd
}

func socialOffCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "off [reason]",
		Short: "Stop posting, now",
		Long: "Takes effect on the next post — no restart. The reason is shown back\n" +
			"in the refusal, so you can tell later why you switched it off.",
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			reason := strings.Join(args, " ")
			if reason == "" {
				reason = "switched off manually"
			}
			db, err := openStore()
			if err != nil {
				return err
			}
			defer db.Close()
			if err := db.KVSet("social", "posting_off", reason, 0); err != nil {
				return err
			}
			fmt.Printf("Posting is off: %s\nTurn it back on with `karmax social on`.\n", reason)
			return nil
		},
	}
}

func socialOnCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "on",
		Short: "Allow posting again",
		RunE: func(cmd *cobra.Command, args []string) error {
			db, err := openStore()
			if err != nil {
				return err
			}
			defer db.Close()
			if err := db.KVDelete("social", "posting_off"); err != nil {
				return err
			}
			fmt.Println("Posting is on.")
			return nil
		},
	}
}

func socialLogCmd() *cobra.Command {
	var limit int
	cmd := &cobra.Command{
		Use:   "log",
		Short: "What KARMAX has posted, and what it refused to post",
		RunE: func(cmd *cobra.Command, args []string) error {
			db, err := openStore()
			if err != nil {
				return err
			}
			defer db.Close()
			posts, err := db.RecentPosts(limit)
			if err != nil {
				return err
			}
			if len(posts) == 0 {
				fmt.Println("Nothing has been posted yet.")
				return nil
			}
			for _, p := range posts {
				fmt.Printf("%s  %-8s  %s\n", p.PostedAt.Local().Format("2 Jan 15:04"), p.Status, p.Platform)
				fmt.Printf("  %s\n", oneLine(p.Text, 140))
				if p.Detail != "" {
					fmt.Printf("  → %s\n", oneLine(p.Detail, 140))
				}
				fmt.Println()
			}
			return nil
		},
	}
	cmd.Flags().IntVar(&limit, "limit", 20, "how many to show")
	return cmd
}

func socialStatus() error {
	db, err := openStore()
	if err != nil {
		return err
	}
	defer db.Close()

	if reason, found, _ := db.KVGet("social", "posting_off"); found && reason != "" {
		fmt.Printf("Posting is OFF — %s\n\n", reason)
	} else {
		fmt.Println("Posting is on.")
		fmt.Println()
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "PLATFORM\tLAST 24H\tLAST POST")
	for _, platform := range []string{"x", "linkedin"} {
		n, _ := db.CountPostsSince(platform, time.Now().Add(-24*time.Hour))
		last, _ := db.LastPostAt(platform)
		when := "never"
		if !last.IsZero() {
			when = last.Local().Format("2 Jan 15:04")
		}
		fmt.Fprintf(w, "%s\t%d\t%s\n", platform, n, when)
	}
	w.Flush()
	fmt.Println("\n`karmax social log` shows what was said, `karmax social off` stops it.")
	return nil
}
