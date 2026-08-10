//go:build wasip1

// A guest that tries to escape, so the host's claims can be checked rather
// than believed. Every breach it manages announces itself in the log, and the
// test fails on the word BREACH.
package main

import (
	"os"

	"github.com/MelloB1989/karmax/pkg/loopwasm"
)

//go:wasmexport run
func run() {
	loopwasm.Log("guest started")

	// The filesystem. There is no FS in the module config, so preview1's
	// path_open has nothing to open.
	if b, err := os.ReadFile("/etc/passwd"); err == nil {
		loopwasm.Log("BREACH: read %d bytes of /etc/passwd", len(b))
	} else {
		loopwasm.Log("checked: no filesystem")
	}
	if _, err := os.ReadDir("/"); err == nil {
		loopwasm.Log("BREACH: listed the root directory")
	} else {
		loopwasm.Log("checked: cannot list directories")
	}
	if f, err := os.Create("/tmp/karmax-escape"); err == nil {
		f.Close()
		loopwasm.Log("BREACH: created a file")
	} else {
		loopwasm.Log("checked: cannot create files")
	}

	// The environment. This is the one that matters most: the daemon's process
	// holds API keys, and an inherited environ would hand them over.
	if env := os.Environ(); len(env) > 0 {
		loopwasm.Log("BREACH: environment has %d entries, first %s", len(env), env[0])
	} else {
		loopwasm.Log("checked: no environment")
	}

	// A host function this loop's manifest did not declare.
	if _, err := loopwasm.Recall("anything at all", 1); err == nil {
		loopwasm.Log("BREACH: called an undeclared host function")
	} else {
		loopwasm.Log("checked: undeclared host function refused")
	}

	// A host function it DID declare, to prove the sandbox is not simply broken.
	loopwasm.Log("checked: declared host function works")
}

func main() {}
