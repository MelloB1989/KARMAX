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
	"time"

	"github.com/MelloB1989/karmax/fleet/internal/config"
	"github.com/MelloB1989/karmax/fleet/internal/fleetapi"
	"github.com/MelloB1989/karmax/fleet/internal/tailnet"
)

// WhoIs says who is behind a tailnet address: tailnet.Local.
type WhoIs interface {
	WhoIs(ctx context.Context, addr string) (tailnet.Who, error)
}

// Tailnet is the API for people on the Tailscale network. Each request is
// from the device tailscaled says its address is, with that device's tag
// (devices): an admin device has the full scope, a guest device its lent
// agents; a token in it counts for nothing. A device with no tag is refused,
// told how to ask, and its knock recorded. Any tailnet person may get
// fleetctl itself: GET /install.sh (macOS, Linux), /install.ps1 (Windows)
// and the builds in cfg.Dist at /dl/. The scripts also register fleetctl's
// MCP server with Claude Code, when it is installed.
func Tailnet(api http.Handler, who WhoIs, devices Devices, cfg *config.Tailnet) http.Handler {
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
	mux.Handle("/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		wh := r.Context().Value(whoKey{}).(tailnet.Who)
		d, err := devices.ResolveDevice(wh, time.Now())
		if err != nil { // the store unreadable: nobody on the tailnet, loopback still works
			writeErr(w, http.StatusServiceUnavailable, fmt.Errorf("fleetd cannot read its access list: %v", err))
			return
		}
		if d.Role == "" {
			refuseDevice(w, devices, wh)
			return
		}
		c := &Caller{Login: wh.Login, Name: wh.Name, Role: string(d.Role), MaxOpen: cfg.GuestOpen,
			Device: wh.Node, StableID: wh.StableID}
		if d.Role == fleetapi.RoleGuest {
			c.Lent = d.Agents
		}
		api.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), callerKey{}, c)))
	}))
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
		mux.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), whoKey{}, wh)))
	})
}

type whoKey struct{}

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
# Installs fleetctl for the fleet at {{URL}}, connects it, installs its
# Claude Code skills and registers its MCP server with Claude Code.
# Run: curl -fsSL {{URL}}/install.sh | sh
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
# The fleet MCP server, in Claude Code's user config: left alone when it is
# this fleetctl already, replaced when it is another path. Every claude
# reads /dev/null, not this script.
add="claude mcp add --scope user fleet -- $dir/fleetctl mcp"
if command -v claude >/dev/null 2>&1; then
	if claude mcp get fleet </dev/null 2>/dev/null | (
		while IFS= read -r line; do
			case $line in *"Command: $dir/fleetctl") exit 0 ;; esac
		done
		exit 1
	); then
		echo "Claude Code already has the fleet MCP server: $dir/fleetctl mcp"
	else
		claude mcp remove --scope user fleet </dev/null >/dev/null 2>&1 || true
		if claude mcp add --scope user fleet -- "$dir/fleetctl" mcp </dev/null >/dev/null; then
			echo "registered the fleet MCP server with Claude Code: $dir/fleetctl mcp"
		else
			echo "claude could not register the fleet MCP server; run later:  $add" >&2
		fi
	fi
else
	echo "claude was not found: once Claude Code is installed, run  $add"
fi
case ":$PATH:" in
*":$dir:"*) ;;
*) echo "$dir is not on your PATH: add  export PATH=\"$dir:\$PATH\"  to your shell's profile" ;;
esac
`

const installPs1 = `# Installs fleetctl for the fleet at {{URL}}, connects it, installs its
# Claude Code skills and registers its MCP server with Claude Code.
# Run in PowerShell: irm {{URL}}/install.ps1 | iex
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
# The fleet MCP server, in Claude Code's user config: left alone when it is
# this fleetctl already, replaced when it is another path. '--' is quoted so
# PowerShell passes it on to claude (an npm shim is a script).
$add = "claude mcp add --scope user fleet -- $exe mcp"
if (Get-Command claude -ErrorAction SilentlyContinue) {
  $ErrorActionPreference = 'Continue'
  $got = (& claude mcp get fleet 2>$null | Out-String)
  if ($got -match ('(?m)Command: ' + [regex]::Escape($exe) + '\s*$')) {
    Write-Host "Claude Code already has the fleet MCP server: $exe mcp"
  } else {
    & claude mcp remove --scope user fleet 2>$null | Out-Null
    & claude mcp add --scope user fleet '--' $exe mcp | Out-Null
    if ($LASTEXITCODE -eq 0) {
      Write-Host "registered the fleet MCP server with Claude Code: $exe mcp"
    } else {
      Write-Host "claude could not register the fleet MCP server; run later:  $add"
    }
  }
  $ErrorActionPreference = 'Stop'
} else {
  Write-Host "claude was not found: once Claude Code is installed, run  $add"
}
Write-Host "fleetctl installed in $dir; open a new terminal to have it on your PATH"
`
