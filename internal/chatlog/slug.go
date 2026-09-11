// Package chatlog reads the conversations Claude Code already keeps.
//
// KARMAX stores no chat history of its own. A conversation IS a harness
// session: the CLI writes the transcript, titles it, and compacts it, at
// ~/.claude/projects/<slug>/<session-id>.jsonl. This package is the only
// place that knows that, so the day the format moves there is one file to fix.
//
// Every reader here skips record types it does not recognise. The format
// belongs to another program and gains types without asking; a reader that
// errors on an unknown line would break on somebody else's release.
package chatlog

import (
	"os"
	"path/filepath"
	"strings"
)

var slugger = strings.NewReplacer("/", "-", ".", "-", "_", "-")

// Slug turns a working directory into the directory name the CLI uses for it.
func Slug(workdir string) string { return slugger.Replace(workdir) }

// Dir is where a working directory's sessions live.
func Dir(workdir string) string {
	return filepath.Join(homeDir(), ".claude", "projects", Slug(workdir))
}

func homeDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "."
	}
	return home
}
