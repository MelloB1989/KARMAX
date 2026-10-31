package runtime

import (
	"os"
	"path/filepath"
	"strings"
)

// harnessSkillDirs turns harness.skills entries into skill directories: a
// directory as written (~ expanded), or the name of one of the operator's own
// skills, in Claude Code's skills directory. nil stays nil and an empty list
// stays empty, which is the difference between a kind keeping the default and
// a kind asking for none.
func harnessSkillDirs(entries []string) []string {
	if entries == nil {
		return nil
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		e = strings.TrimSpace(e)
		switch {
		case e == "":
		case isPathEntry(e):
			out = append(out, expandHome(e))
		default:
			out = append(out, filepath.Join(claudeConfigDir(), "skills", e))
		}
	}
	return out
}

// harnessPluginRefs keeps an installed plugin's id (name@marketplace) as it
// is and expands ~ in a plugin directory; nil and empty as harnessSkillDirs.
func harnessPluginRefs(entries []string) []string {
	if entries == nil {
		return nil
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		e = strings.TrimSpace(e)
		switch {
		case e == "":
		case isPathEntry(e):
			out = append(out, expandHome(e))
		default:
			out = append(out, e)
		}
	}
	return out
}

func isPathEntry(e string) bool {
	return strings.HasPrefix(e, "~") || strings.ContainsRune(e, '/') || strings.ContainsRune(e, filepath.Separator)
}

func expandHome(p string) string {
	if p != "~" && !strings.HasPrefix(p, "~/") {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return p
	}
	return filepath.Join(home, strings.TrimPrefix(p, "~"))
}

// claudeConfigDir is where the CLI keeps the operator's own config: the
// sessions inherit KARMAX's environment, CLAUDE_CONFIG_DIR included.
func claudeConfigDir() string {
	if d := strings.TrimSpace(os.Getenv("CLAUDE_CONFIG_DIR")); d != "" {
		return expandHome(d)
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".claude")
}
