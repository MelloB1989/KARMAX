//go:build wasip1

// A guest that PROVIDES tools, to check that the host can call back into a
// module and get an answer out of it.
package main

import (
	"fmt"

	"github.com/MelloB1989/karmax/pkg/loopwasm"
)

// Registered in init, not in run: the host serves a provided tool on a FRESH
// instance where run() has never executed. A loop that registered its handlers
// in run would answer "no such tool" to every call.
func init() {
	loopwasm.Provide("deal.status", func(in struct {
		Deal string `json:"deal"`
	}) (string, error) {
		if in.Deal == "" {
			return "", fmt.Errorf("a deal is required")
		}
		return "the " + in.Deal + " deal is at stage 3", nil
	})

	loopwasm.Provide("always.fails", func(struct{}) (string, error) {
		return "", fmt.Errorf("this tool is broken on purpose")
	})

	// Proves the fresh instance really is fresh: run() sets this, and a tool
	// call must never see it set.
	loopwasm.Provide("saw.run", func(struct{}) (string, error) {
		if ranAlready {
			return "run() had executed in this instance", nil
		}
		return "fresh", nil
	})
}

var ranAlready bool

//go:wasmexport run
func run() {
	ranAlready = true
	loopwasm.Log("provider ran")
}

func main() {}
