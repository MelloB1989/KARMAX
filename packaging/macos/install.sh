#!/usr/bin/env bash
# KARMAX — macOS installer.
#
# Installs the karmax binary and a launchd LaunchAgent that runs KARMAX
# AGGRESSIVELY in the background: KeepAlive relaunches it the instant it exits
# or crashes, and RunAtLoad starts it at login.
#
# Run this from the extracted release directory (it installs the sibling
# `karmax` binary), or via the one-liner:
#   curl -fsSL https://github.com/MelloB1989/KARMAX/releases/latest/download/install.sh | bash
#
# Overrides: KARMAX_PREFIX (install dir, default ~/.local/bin),
#            KARMAX_DATA_DIR (data dir, default ~/.karmax).
set -euo pipefail

SELF_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PREFIX="${KARMAX_PREFIX:-$HOME/.local/bin}"
DATA_DIR="${KARMAX_DATA_DIR:-$HOME/.karmax}"
LABEL="in.mellob.karmax"
AGENTS="$HOME/Library/LaunchAgents"
PLIST="$AGENTS/$LABEL.plist"
UID_NUM="$(id -u)"

# Locate the karmax binary: next to this script (release archive), at the repo
# root when run from a source checkout after `make build`, or in the cwd.
BIN_SRC=""
for c in "$SELF_DIR/karmax" "$SELF_DIR/../../karmax" "$PWD/karmax"; do
	if [ -f "$c" ]; then BIN_SRC="$(cd "$(dirname "$c")" && pwd)/$(basename "$c")"; break; fi
done
[ -n "$BIN_SRC" ] || { echo "error: karmax binary not found — build it with 'make build' or run from the release archive" >&2; exit 1; }

echo "==> installing karmax -> $PREFIX/karmax"
mkdir -p "$PREFIX" "$DATA_DIR/logs" "$AGENTS"
install -m 0755 "$BIN_SRC" "$PREFIX/karmax"
# A binary downloaded from a Release is quarantined by Gatekeeper; clear the
# attribute so launchd is allowed to execute it.
xattr -dr com.apple.quarantine "$PREFIX/karmax" 2>/dev/null || true

if [ ! -f "$DATA_DIR/karmax.yaml" ]; then
	"$PREFIX/karmax" init >/dev/null 2>&1 || true
fi

echo "==> writing launchd agent -> $PLIST"
cat > "$PLIST" <<EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key><string>$LABEL</string>
	<key>ProgramArguments</key>
	<array>
		<string>$PREFIX/karmax</string>
		<string>start</string>
	</array>
	<key>WorkingDirectory</key><string>$DATA_DIR</string>
	<key>RunAtLoad</key><true/>
	<key>KeepAlive</key><true/>
	<key>ThrottleInterval</key><integer>2</integer>
	<key>ProcessType</key><string>Background</string>
	<key>StandardOutPath</key><string>$DATA_DIR/logs/karmax.out.log</string>
	<key>StandardErrorPath</key><string>$DATA_DIR/logs/karmax.err.log</string>
	<key>EnvironmentVariables</key>
	<dict>
		<key>PATH</key><string>$HOME/.local/bin:/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin</string>
	</dict>
</dict>
</plist>
EOF

# (Re)load the agent into the current GUI session.
launchctl bootout "gui/$UID_NUM/$LABEL" 2>/dev/null || true
launchctl bootstrap "gui/$UID_NUM" "$PLIST"
launchctl enable "gui/$UID_NUM/$LABEL"
launchctl kickstart -k "gui/$UID_NUM/$LABEL"

echo
echo "karmax is installed and running (launchd keeps it alive)."
launchctl print "gui/$UID_NUM/$LABEL" 2>/dev/null | grep -E '(^|[[:space:]])(state|pid) =' | head -n 2 || true
echo
echo "  logs:    tail -f $DATA_DIR/logs/karmax.err.log"
echo "  stop:    launchctl bootout gui/$UID_NUM/$LABEL"
case ":$PATH:" in
	*":$PREFIX:"*) : ;;
	*) echo "  note:  add $PREFIX to your PATH to run 'karmax' directly." ;;
esac
