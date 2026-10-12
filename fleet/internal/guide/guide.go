// Package guide holds the text that teaches a Claude session to put the
// fleet to work: printed by `fleetctl guide`, served by fleetd, and installed
// as a Claude Code skill by `fleetctl skill install`.
package guide

import (
	_ "embed"
	"strings"
)

//go:embed guide.md
var text string

const frontmatter = "---\nname: fleet\n" +
	"description: Use when work can be delegated to the standby Claude Code agent fleet — parallel or independent " +
	"coding tasks, \"use the fleet/agents\", fan-out, or long-running work — to run, wait for, review and merge " +
	"agent tasks with fleetctl.\n---\n\n"

// Text is the guide.
func Text() string { return strings.TrimRight(text, "\n") + "\n" }

// Skill is Text as a Claude Code skill: SKILL.md.
func Skill() string { return frontmatter + Text() }
