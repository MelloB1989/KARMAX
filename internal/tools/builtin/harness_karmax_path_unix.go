//go:build !windows

package builtin

import (
	"os"
	"path/filepath"
)

// installKarmaxLink makes <binDir>/karmax a symlink to exe. See
// swapIntoPlace for why it goes through a temp name rather than
// remove-then-symlink.
func installKarmaxLink(binDir, exe string) error {
	link := filepath.Join(binDir, "karmax")
	return swapIntoPlace(link, func(tmp string) error {
		return os.Symlink(exe, tmp)
	})
}
