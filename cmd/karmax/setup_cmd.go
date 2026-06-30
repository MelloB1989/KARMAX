package main

import (
	"fmt"
	"os/exec"
	"strings"

	"github.com/MelloB1989/karmax/internal/loopinstall"
	"github.com/spf13/cobra"
)

// newSetupCmd prepares a machine to install loops: loops are compile-time Go
// plugins, so installing one rebuilds KARMAX. This ensures a Go compiler is
// present and a source checkout exists to rebuild from.
func newSetupCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "setup",
		Short: "Prepare this machine to install loops (ensure Go + clone the source)",
		Long: "Loops are Go plugins compiled into KARMAX, so installing one recompiles the binary.\n" +
			"`karmax setup` makes that possible on a binary-only install: it ensures a Go compiler\n" +
			"(installing the toolchain into ~/.karmax/toolchain if needed) and clones the KARMAX\n" +
			"source (shallow) into ~/.karmax/src.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			fmt.Println("karmax setup")
			fmt.Println("------------")

			fmt.Print("Go compiler:       ")
			goBin, err := loopinstall.EnsureGo()
			if err != nil {
				return fmt.Errorf("Go: %w", err)
			}
			ver, _ := exec.Command(goBin, "version").Output()
			fmt.Printf("OK (%s)\n", strings.TrimSpace(string(ver)))

			fmt.Print("C compiler (cgo):  ")
			switch {
			case commandOnPath("gcc"):
				fmt.Println("OK (gcc)")
			case commandOnPath("cc"):
				fmt.Println("OK (cc)")
			default:
				fmt.Println("MISSING — install one (e.g. apt install build-essential); KARMAX uses cgo for SQLite")
			}

			fmt.Print("Source workspace:  ")
			root, err := loopinstall.EnsureWorkspace()
			if err != nil {
				return fmt.Errorf("workspace: %w", err)
			}
			fmt.Printf("OK (%s)\n", root)

			fmt.Println("\nReady — install loops with `karmax loops`.")
			return nil
		},
	}
}

func commandOnPath(name string) bool {
	_, err := exec.LookPath(name)
	return err == nil
}
