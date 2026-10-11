package console

import (
	"embed"
	"io/fs"
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
