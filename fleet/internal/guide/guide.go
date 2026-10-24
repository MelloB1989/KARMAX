// Package guide holds the text that teaches a Claude session to put the
// fleet to work: printed by `fleetctl guide`, served by fleetd, and installed
// as a Claude Code skill by `fleetctl skill install`, with the fleet-memory
// skill beside it.
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

//go:embed memory.md
var memoryText string

const memoryFrontmatter = "---\nname: fleet-memory\n" +
	"description: Use before starting work on a person, project or decision the operator has history with, and when " +
	"you learn a durable fact worth keeping: recall the operator's long-term memory and suggest additions with " +
	"fleetctl memory (or the fleet MCP server's memory_recall / memory_suggest).\n---\n\n"

// MemorySkill teaches recalling the operator's memory and suggesting to it:
// the fleet-memory skill, installed beside the fleet skill.
func MemorySkill() string { return memoryFrontmatter + strings.TrimRight(memoryText, "\n") + "\n" }

// IsMemorySkill is whether a SKILL.md is the fleet-memory skill fleetctl
// writes (any version of its text), not one of someone's own.
func IsMemorySkill(text string) bool { return strings.HasPrefix(text, memoryFrontmatter) }
