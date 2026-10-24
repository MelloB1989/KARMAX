package main

import (
	"os"
	"strings"
)

// sessionName is who fleetctl speaks as when it is not told (--from): a name
// that replies, reports and questions are filed under.
//
//  1. FLEET_SESSION, a name the session chose for itself.
//  2. The fleet container's own name (FLEET_NAME, FLEET_AGENT): an agent or
//     the orchestrator.
//  3. The Claude Code session fleetctl runs inside: "cc-" and the first eight
//     characters of CLAUDE_CODE_SESSION_ID, which Claude Code sets for every
//     Bash command of a session and keeps for the session's life.
//  4. operator.
func sessionName() string {
	for _, v := range []string{"FLEET_SESSION", "FLEET_NAME", "FLEET_AGENT"} {
		if n := strings.TrimSpace(os.Getenv(v)); n != "" {
			return n
		}
	}
	if id := strings.TrimSpace(os.Getenv("CLAUDE_CODE_SESSION_ID")); id != "" {
		id = strings.ReplaceAll(strings.ToLower(id), "-", "")
		return "cc-" + id[:min(len(id), 8)]
	}
	return "operator"
}
