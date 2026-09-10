package main

import (
	"context"
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/MelloB1989/karmax/internal/browser"
	"github.com/MelloB1989/karmax/internal/config"
	"github.com/MelloB1989/karmax/internal/hostpaths"
	"github.com/spf13/cobra"
)

// The browser, from a terminal.
//
// Mostly so the daemon is not the only way to reach it: somebody debugging why
// a connector will not authorize needs to be able to look at the window and
// see what is actually on it.

func browserCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "browser",
		Short: "The browser you and the assistant share",
		Long: "One Chromium, with a profile KARMAX owns, that you sign into and the assistant then\n" +
			"drives. Sign into Google, Instagram, LinkedIn — whatever you want it to reach — in\n" +
			"this window, and it will find those sessions already there.",
	}
	cmd.AddCommand(browserStatusCmd(), browserStartCmd(), browserOpenCmd(), browserStopCmd())
	return cmd
}

func session() *browser.Session {
	dir := ""
	if cfg, err := config.Load(findConfig()); err == nil {
		dir = cfg.Karmax.DataDir
	}
	return browser.Shared(dir)
}

func browserStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Whether it is open, and what is on it",
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := context.Background()
			s := session()
			bin := hostpaths.Browser()
			if bin == "" {
				fmt.Println("No Chrome, Chromium or Edge on this machine.")
				return nil
			}
			fmt.Println("browser: ", bin)
			fmt.Println("profile: ", s.Profile())
			if !s.Running(ctx) {
				fmt.Println("status:   not running")
				return nil
			}
			fmt.Println("status:   running")
			tabs, err := s.Tabs(ctx)
			if err != nil || len(tabs) == 0 {
				return nil
			}
			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "\nTITLE\tURL")
			for _, t := range tabs {
				fmt.Fprintf(w, "%s\t%s\n", t.Title, t.URL)
			}
			return w.Flush()
		},
	}
}

func browserStartCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "start",
		Short: "Open it",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := session().Start(context.Background()); err != nil {
				return err
			}
			fmt.Println("Open. Sign into whatever you want the assistant to reach.")
			return nil
		},
	}
}

func browserOpenCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "open <url>",
		Short: "Put a page in front of you",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			tab, err := session().Open(context.Background(), args[0])
			if err != nil {
				return err
			}
			fmt.Println("Opened", tab.URL)
			return nil
		},
	}
}

func browserStopCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "stop",
		Short: "Close it (your sign-ins are kept)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := session().Stop(context.Background()); err != nil {
				return err
			}
			fmt.Println("Closed. What you signed into is still there next time.")
			return nil
		},
	}
}
