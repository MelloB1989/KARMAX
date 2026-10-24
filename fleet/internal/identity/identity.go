// Package identity is a stand-in for the verified agent identity on
// fleet/agent-03/v2-memory (task v2-memory): only Base, which the KARMAX
// gateway uses. At merge, that branch's package replaces this file.
package identity

import (
	"regexp"
	"strings"
)

var workerSuffix = regexp.MustCompile(`-s[0-9]+$`)

// Base maps a worker name to its container's agent: "agent-03:2" and
// "agent-03-s2" are both agent-03.
func Base(name string) string {
	name, _, _ = strings.Cut(name, ":")
	return workerSuffix.ReplaceAllString(name, "")
}
