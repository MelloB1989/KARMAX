package builtin

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// harnessCmdEnv is the environment a spawned `claude` runs with: the
// allowlist from safety.HarnessEnv(), plus `karmax` made resolvable so a
// harness can call back into its own engine.
//
// The karmax-* skills installed for the harness tell it to run bare `karmax
// ...` commands (`karmax browser start`, `karmax memory search`, ...). The
// running engine process is never named that — it's karmax-darwin-arm64, or
// Resources/core/karmax-<os>-<arch> in the packaged app — and its directory
// isn't on PATH, so without this every such command fails "command not
// found". harnessKarmaxBinDir/prependKarmaxToPath below fix that generically:
// a stable `karmax` entry under <DataDir>/bin, pointing at whatever binary is
// currently running this process, prepended onto (never replacing) the
// allowlisted PATH.
func (t *ClaudeCodeTool) harnessCmdEnv() []string {
	return prependKarmaxToPath(harnessEnv(), harnessKarmaxBinDir(t.DataDir))
}

// harnessKarmaxBinDir ensures <dataDir>/bin holds a `karmax` entry (a
// symlink on unix, a .cmd shim on windows — see installKarmaxLink) pointing
// at the currently-running engine binary, and returns that directory.
//
// Best-effort: any failure along the way (can't resolve the running
// executable, can't create or write the directory) is reported to stderr and
// this returns "", so the caller leaves PATH exactly as harnessEnv() built
// it rather than breaking the harness turn over a convenience feature.
func harnessKarmaxBinDir(dataDir string) string {
	exe, err := os.Executable()
	if err != nil {
		fmt.Fprintf(os.Stderr, "karmax: could not resolve the running executable for the harness PATH: %v\n", err)
		return ""
	}
	if real, err := filepath.EvalSymlinks(exe); err == nil {
		exe = real
	}

	dataDir = strings.TrimSpace(dataDir)
	if dataDir == "" {
		// Same fallback as fsscope.Path and loopinstall.DataDir: DataDir is
		// normally always set (internal/config defaults it to ~/.karmax),
		// but a caller that leaves it blank still gets the same place
		// everything else in KARMAX calls home.
		home, err := os.UserHomeDir()
		if err != nil {
			fmt.Fprintf(os.Stderr, "karmax: could not resolve home dir for the harness PATH: %v\n", err)
			return ""
		}
		dataDir = filepath.Join(home, ".karmax")
	}

	binDir := filepath.Join(dataDir, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "karmax: could not create %s for the harness PATH: %v\n", binDir, err)
		return ""
	}

	if err := installKarmaxLink(binDir, exe); err != nil {
		fmt.Fprintf(os.Stderr, "karmax: could not refresh the karmax entry in %s: %v\n", binDir, err)
		return ""
	}
	return binDir
}

// prependKarmaxToPath returns env with binDir prepended to PATH, merging
// with whatever harnessEnv() already allowed through rather than replacing
// it — safety.HarnessEnv's allowlist is untouched by this, PATH included.
// A blank binDir (harnessKarmaxBinDir failed) is a no-op.
func prependKarmaxToPath(env []string, binDir string) []string {
	if binDir == "" {
		return env
	}
	out := make([]string, len(env))
	copy(out, env)
	for i, kv := range out {
		if rest, ok := strings.CutPrefix(kv, "PATH="); ok {
			out[i] = "PATH=" + binDir + string(os.PathListSeparator) + rest
			return out
		}
	}
	// harnessEnv() had no PATH entry at all (e.g. it wasn't set in the
	// engine's own environment) — still give the harness one.
	return append(out, "PATH="+binDir)
}

// swapIntoPlace writes body to path atomically: to a temp file in the same
// directory, then renamed over path. A harness resolving `karmax` mid-
// refresh — another harness turn starting while this one rewrites the link —
// never sees a missing file, and a refresh that fails partway never leaves a
// corrupt one. Shared by both platforms' installKarmaxLink.
func swapIntoPlace(path string, write func(tmp string) error) error {
	tmp := path + ".tmp"
	if err := write(tmp); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}
