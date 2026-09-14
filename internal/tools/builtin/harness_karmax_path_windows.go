//go:build windows

package builtin

import (
	"os"
	"path/filepath"
)

// installKarmaxLink writes <binDir>/karmax.cmd, a shim that execs exe with
// its own arguments. Windows symlinks need SeCreateSymbolicLinkPrivilege or
// Developer Mode, neither of which this process can assume it has, so a
// batch shim is what makes `karmax ...` resolve here instead — exec.LookPath
// (and cmd.exe itself) tries each PATHEXT extension against a bare name, and
// .CMD is in PATHEXT by default on every Windows install.
func installKarmaxLink(binDir, exe string) error {
	shim := filepath.Join(binDir, "karmax.cmd")
	body := "@echo off\r\n\"" + exe + "\" %*\r\n"
	return swapIntoPlace(shim, func(tmp string) error {
		return os.WriteFile(tmp, []byte(body), 0o755)
	})
}
