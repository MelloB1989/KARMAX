package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/MelloB1989/karmax/fleet/internal/fleetapi"
)

// cleanupCmd is `fleetctl cleanup [--dry-run] [--json]`: a cleanup pass now
// (POST /v1/cleanup), the one fleetd runs every cleanup.every on its own.
func cleanupCmd(args []string) error {
	var dry, asJSON bool
	_, pos, err := flags("cleanup", args, func(fs *flag.FlagSet) {
		fs.BoolVar(&dry, "dry-run", false, "")
		fs.BoolVar(&asJSON, "json", false, "")
	})
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return errors.New("usage: fleetctl cleanup [--dry-run] [--json]")
	}
	c, err := newClient()
	if err != nil {
		return err
	}
	c.timeout = 75 * time.Minute // a pass walks every agent's work and caches; fleetd bounds it at an hour
	var rep fleetapi.CleanupReport
	if err := c.do("POST", fleetapi.PathCleanup, fleetapi.CleanupRequest{DryRun: dry}, &rep); err != nil {
		return err
	}
	if asJSON {
		return printJSON(rep)
	}
	printCleanup(os.Stdout, rep)
	return nil
}

// printCleanup writes what a pass deleted or kept, where it did not look,
// and what it freed.
func printCleanup(w io.Writer, rep fleetapi.CleanupReport) {
	if len(rep.Items) > 0 {
		tw := tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
		fmt.Fprintln(tw, "WHERE\tACTION\tSIZE\tPATH\tWHY")
		for _, it := range rep.Items {
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", it.Where, strings.ReplaceAll(it.Action, "_", " "), sizeOf(it.Bytes), it.Path, it.Why)
		}
		_ = tw.Flush()
	}
	if len(rep.Skipped) > 0 {
		if len(rep.Items) > 0 {
			fmt.Fprintln(w)
		}
		fmt.Fprintln(w, "Not looked at:")
		tw := tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
		for _, s := range rep.Skipped {
			fmt.Fprintf(tw, "  %s\t%s\n", s.Where, s.Why)
		}
		_ = tw.Flush()
	}
	if len(rep.Items) > 0 || len(rep.Skipped) > 0 {
		fmt.Fprintln(w)
	}
	if rep.DryRun {
		fmt.Fprintf(w, "Would free %s (dry run: nothing deleted). Unused for %s, an item goes.\n", sizeOf(rep.Freed), rep.Stale)
		return
	}
	fmt.Fprintf(w, "Freed %s. Unused for %s, an item goes.\n", sizeOf(rep.Freed), rep.Stale)
}

// sizeOf is a byte count for people: 512 B, 3.4 MiB, 2.0 GiB.
func sizeOf(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	v, i := float64(n), -1
	for v >= unit && i < 4 {
		v /= unit
		i++
	}
	return fmt.Sprintf("%.1f %ciB", v, "KMGTP"[i])
}
