package builtin

import "github.com/MelloB1989/karmax/internal/safety"

// harnessEnv is the environment a spawned harness runs with: an allowlist, so a
// secret nobody thought of does not reach a subprocess that runs with
// permissions skipped. See internal/safety.
func harnessEnv() []string { return safety.HarnessEnv() }
