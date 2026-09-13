// Package skills ships KARMAX's built-in skills inside the binary and
// materialises them under the operator's profile so a harness can load them
// via --plugin-dir.
package skills

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
)

// Without an `all:` prefix this silently skips anything under assets whose
// name starts with "." or "_" — so a skill file named that way would not ship
// and nothing would say so. Kept deliberately: it is also what keeps a
// stray __pycache__ beside the Python scripts out of the binary.
//
//go:embed assets
var assets embed.FS

const assetsRoot = "assets"

// shippedManifest records the hash of what this binary ships for each skill
// file, so a future version can tell "absent" from "operator edited" without
// ever needing to overwrite a file to find out.
const shippedManifest = ".shipped.json"

// Materialise writes the embedded skills under <dataDir>/skills, creating a
// file only when it is absent — an operator's edit (or deletion) of a shipped
// file must survive every restart. It returns the skills directory, for
// harness.Options.PluginDir.
func Materialise(dataDir string) (string, error) {
	skillsDir := filepath.Join(dataDir, "skills")
	if err := os.MkdirAll(skillsDir, 0o755); err != nil {
		return "", err
	}

	shipped := map[string]string{}
	err := fs.WalkDir(assets, assetsRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(assetsRoot, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)

		content, err := assets.ReadFile(path)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(content)
		shipped[rel] = hex.EncodeToString(sum[:])

		target := filepath.Join(skillsDir, filepath.FromSlash(rel))
		switch _, statErr := os.Stat(target); {
		case statErr == nil:
			return nil // present already, edited or not — never touched
		case !os.IsNotExist(statErr):
			return statErr
		}

		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		return os.WriteFile(target, content, 0o644)
	})
	if err != nil {
		return "", err
	}

	manifest, err := json.MarshalIndent(shipped, "", "  ")
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(skillsDir, shippedManifest), manifest, 0o644); err != nil {
		return "", err
	}

	return skillsDir, nil
}
