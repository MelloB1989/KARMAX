package dashboards

import (
	"os"
	"path/filepath"
	"strings"
)

// referenceFallback is what 'components' answers on an instance nobody has
// installed a kit on. Without it, an agent asking "what should this look
// like" gets a file-not-found and has to guess whether that means "write
// anything" or "something is broken" — this makes the first answer explicit.
const referenceFallback = "No component kit is installed on this instance. " +
	"Write the dashboard as plain, self-contained HTML with inline <style> — no external framework or asset is available to it."

func referencePath() string { return filepath.Join(Root(), "_kit", "REFERENCE.md") }

// ComponentReference returns the installed kit's reference doc, or the
// fallback if none is installed.
func ComponentReference() string {
	b, err := os.ReadFile(referencePath())
	if err != nil || strings.TrimSpace(string(b)) == "" {
		return referenceFallback
	}
	return string(b)
}

// WriteComponentReference installs (or replaces) the kit's reference doc.
func WriteComponentReference(text string) error {
	return atomicWrite(referencePath(), []byte(text))
}
