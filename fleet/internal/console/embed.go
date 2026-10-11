package console

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"io/fs"
	"sync"
)

// dist is the built panel (npm run build in fleet/console). Only .gitignore
// and .keep are tracked, so a fresh checkout embeds no index.html and the
// server says so instead of serving nothing.
//
//go:embed all:dist
var distFS embed.FS

func embeddedAssets() fs.FS {
	sub, err := fs.Sub(distFS, "dist")
	if err != nil {
		panic(err) // dist is embedded at build time
	}
	return sub
}

const notBuiltPage = `<!doctype html><meta charset="utf-8"><title>fleet console</title>
<body style="background:#05080C;color:#D6E2EC;font:16px monospace;padding:2em">
<p>panel not built — <code>npm ci &amp;&amp; npm run build</code> in <code>fleet/console</code>, then rebuild fleet-console.</p>
`

var (
	buildOnce sync.Once
	buildID   string
)

// BuildID names this build: the version plus a hash of the embedded panel, so
// a rebuilt panel changes it even when Version is still "dev". A kiosk that
// loaded the panel under another id is running a stale bundle and reloads.
func BuildID() string {
	buildOnce.Do(func() { buildID = buildIDOf(Version, embeddedAssets()) })
	return buildID
}

func buildIDOf(version string, assets fs.FS) string {
	h := sha256.New()
	_ = fs.WalkDir(assets, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || d.Name() == ".gitignore" || d.Name() == ".keep" {
			return nil
		}
		b, _ := fs.ReadFile(assets, p)
		fmt.Fprintf(h, "%s\x00%d\x00", p, len(b))
		h.Write(b)
		return nil
	})
	return version + "-" + hex.EncodeToString(h.Sum(nil))[:12]
}
