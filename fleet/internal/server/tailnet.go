package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/MelloB1989/karmax/fleet/internal/config"
	"github.com/MelloB1989/karmax/fleet/internal/tailnet"
)

// WhoIs says who is behind a tailnet address: tailnet.Local.
type WhoIs interface {
	WhoIs(ctx context.Context, addr string) (tailnet.Who, error)
}

// Tailnet is the API for people on the Tailscale network. Each request is
// from whoever tailscaled says its address belongs to, with that login's
// role in cfg; a token in it counts for nothing. It also hands people
// fleetctl itself: GET /install.sh (macOS, Linux), /install.ps1 (Windows)
// and the builds in cfg.Dist at /dl/.
func Tailnet(api http.Handler, who WhoIs, cfg *config.Tailnet) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /install.sh", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/x-shellscript; charset=utf-8")
		_, _ = fmt.Fprint(w, strings.ReplaceAll(installSh, "{{URL}}", baseURL(r, cfg)))
	})
	mux.HandleFunc("GET /install.ps1", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = fmt.Fprint(w, strings.ReplaceAll(installPs1, "{{URL}}", baseURL(r, cfg)))
	})
	mux.HandleFunc("GET /dl/{file}", func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("file")
		p := filepath.Join(cfg.Dist, name)
		if !buildName.MatchString(name) {
			writeErr(w, http.StatusNotFound, fmt.Errorf("no such build %q", name))
			return
		}
		if _, err := os.Stat(p); err != nil {
			writeErr(w, http.StatusNotFound, fmt.Errorf("no %s here yet: the fleet's operator builds them with fleet/scripts/dist.sh", name))
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		http.ServeFile(w, r, p)
	})
	mux.Handle("/", api)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			api.ServeHTTP(w, r)
			return
		}
		wh, err := who.WhoIs(r.Context(), r.RemoteAddr)
		switch {
		case errors.Is(err, tailnet.ErrNotPeer):
			writeErr(w, http.StatusForbidden, fmt.Errorf("%s is not on the tailnet", r.RemoteAddr))
			return
		case err != nil:
			writeErr(w, http.StatusServiceUnavailable, fmt.Errorf("fleetd cannot tell who you are: %v", err))
			return
		case wh.Login == "":
			writeErr(w, http.StatusForbidden, fmt.Errorf("%s is a tagged device: the fleet takes requests from people, not machines", wh.Node))
			return
		}
		role := cfg.Role(wh.Login)
		if role == "" {
			writeErr(w, http.StatusForbidden, fmt.Errorf("%s may not use this fleet: ask its operator to add you (tailnet.members in fleet.yaml)", wh.Login))
			return
		}
		c := &Caller{Login: wh.Login, Name: wh.Name, Role: role, MaxOpen: cfg.MemberOpen}
		mux.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), callerKey{}, c)))
	})
}

// buildName is a fleetctl build dist.sh makes.
var buildName = regexp.MustCompile(`^fleetctl-(linux|darwin|windows)-(amd64|arm64)(\.exe)?$`)

// hostHeader is a Host header safe to put in a script.
var hostHeader = regexp.MustCompile(`^[A-Za-z0-9.-]+(:[0-9]+)?$|^\[[0-9A-Fa-f:]+\](:[0-9]+)?$`)

// baseURL is the address the caller reached fleetd at, which is the one to
// install against: their own name for it (karmax, or the MagicDNS name).
func baseURL(r *http.Request, cfg *config.Tailnet) string {
	if hostHeader.MatchString(r.Host) {
		return "http://" + r.Host
	}
	return "http://" + cfg.Listen
}

const installSh = `#!/bin/sh
# Installs fleetctl for the fleet at {{URL}}, connects it, and installs its
# Claude Code skill. Run: curl -fsSL {{URL}}/install.sh | sh
set -eu
url='{{URL}}'
os=$(uname -s | tr '[:upper:]' '[:lower:]')
case $os in
linux | darwin) ;;
*) echo "fleetctl: no build for $os; on Windows, in PowerShell: irm $url/install.ps1 | iex" >&2; exit 1 ;;
esac
case $(uname -m) in
x86_64 | amd64) arch=amd64 ;;
arm64 | aarch64) arch=arm64 ;;
*) echo "fleetctl: no build for $(uname -m)" >&2; exit 1 ;;
esac
dir=${FLEETCTL_DIR:-}
if [ -z "$dir" ]; then
	case ":$PATH:" in
	*":$HOME/.local/bin:"*) dir=$HOME/.local/bin ;;
	*) if [ -w /usr/local/bin ]; then dir=/usr/local/bin; else dir=$HOME/.local/bin; fi ;;
	esac
fi
mkdir -p "$dir"
tmp=$(mktemp "$dir/.fleetctl.XXXXXX")
trap 'rm -f "$tmp"' EXIT
curl -fsSL "$url/dl/fleetctl-$os-$arch" -o "$tmp"
chmod 755 "$tmp"
mv -f "$tmp" "$dir/fleetctl"
trap - EXIT
echo "fleetctl installed in $dir"
"$dir/fleetctl" connect "$url"
"$dir/fleetctl" skill install --force
case ":$PATH:" in
*":$dir:"*) ;;
*) echo "$dir is not on your PATH: add  export PATH=\"$dir:\$PATH\"  to your shell's profile" ;;
esac
`

const installPs1 = `# Installs fleetctl for the fleet at {{URL}}, connects it, and installs its
# Claude Code skill. Run in PowerShell: irm {{URL}}/install.ps1 | iex
$ErrorActionPreference = 'Stop'
$url = '{{URL}}'
$arch = if ($env:PROCESSOR_ARCHITECTURE -eq 'ARM64') { 'arm64' } else { 'amd64' }
$dir = Join-Path $env:LOCALAPPDATA 'fleetctl'
New-Item -ItemType Directory -Force -Path $dir | Out-Null
$exe = Join-Path $dir 'fleetctl.exe'
Invoke-WebRequest -UseBasicParsing -Uri "$url/dl/fleetctl-windows-$arch.exe" -OutFile $exe
$path = [Environment]::GetEnvironmentVariable('Path', 'User')
if (-not (($path -split ';') -contains $dir)) {
  [Environment]::SetEnvironmentVariable('Path', "$path;$dir", 'User')
  $env:Path += ";$dir"
}
& $exe connect $url
& $exe skill install --force
Write-Host "fleetctl installed in $dir; open a new terminal to have it on your PATH"
`
