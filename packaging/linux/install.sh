#!/usr/bin/env bash
# KARMAX — Linux installer.
#
# Installs the karmax binary and a systemd --user service that runs KARMAX
# AGGRESSIVELY in the background: it restarts on every exit/crash and — with
# linger enabled — keeps running even when you are logged out.
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
UNIT_DIR="$HOME/.config/systemd/user"
CONF_DIR="$HOME/.config/karmax"
UNIT="$UNIT_DIR/karmax.service"

# Locate the karmax binary: next to this script (release archive), at the repo
# root when run from a source checkout after `make build`, or in the cwd.
BIN_SRC=""
for c in "$SELF_DIR/karmax" "$SELF_DIR/../../karmax" "$PWD/karmax"; do
	if [ -f "$c" ]; then BIN_SRC="$(cd "$(dirname "$c")" && pwd)/$(basename "$c")"; break; fi
done
[ -n "$BIN_SRC" ] || { echo "error: karmax binary not found — build it with 'make build' or run from the release archive" >&2; exit 1; }
command -v systemctl >/dev/null 2>&1 || { echo "error: systemd (systemctl) is required" >&2; exit 1; }

echo "==> installing karmax -> $PREFIX/karmax"
mkdir -p "$PREFIX" "$DATA_DIR" "$UNIT_DIR"
install -m 0755 "$BIN_SRC" "$PREFIX/karmax"

# Seed ~/.karmax config on a fresh machine (no-op if it already exists).
if [ ! -f "$DATA_DIR/karmax.yaml" ]; then
	"$PREFIX/karmax" init >/dev/null 2>&1 || true
fi

echo "==> writing systemd --user unit -> $UNIT"
cat > "$UNIT" <<EOF
[Unit]
Description=KARMAX orchestration daemon (personal AI)
Documentation=https://github.com/MelloB1989/KARMAX
After=network-online.target
Wants=network-online.target
# Never give up restarting, even if it crash-loops at boot.
StartLimitIntervalSec=0

[Service]
Type=simple
WorkingDirectory=$DATA_DIR
ExecStart=$PREFIX/karmax start
Restart=always
RestartSec=2
# Persist app stdout+stderr to a flat, greppable log file (rotated below).
StandardOutput=append:$DATA_DIR/karmax.log
StandardError=append:$DATA_DIR/karmax.log
# KARMAX spawns claude/codex/wacli/gws; give them a sensible PATH.
Environment=PATH=$HOME/.local/bin:$HOME/.bun/bin:$HOME/.hermes/node/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin

[Install]
WantedBy=default.target
EOF

# --- log rotation -----------------------------------------------------------
# The unit above appends to a flat file, which (unlike the journal) never trims
# itself. Install a logrotate config + a daily user timer so it can't fill the
# disk. copytruncate is REQUIRED: systemd holds the append fd open, so renaming
# the file would leave the daemon writing into the rotated copy.
if command -v logrotate >/dev/null 2>&1; then
	echo "==> installing log rotation"
	mkdir -p "$CONF_DIR"
	cat > "$CONF_DIR/logrotate.conf" <<EOF
$DATA_DIR/karmax.log $DATA_DIR/wacli.log {
    daily
    rotate 7
    size 20M
    missingok
    notifempty
    compress
    delaycompress
    copytruncate
}
EOF
	cat > "$UNIT_DIR/karmax-logrotate.service" <<EOF
[Unit]
Description=Rotate KARMAX (and wacli) flat logs

[Service]
Type=oneshot
ExecStart=$(command -v logrotate) --state $DATA_DIR/logrotate.state $CONF_DIR/logrotate.conf
EOF
	cat > "$UNIT_DIR/karmax-logrotate.timer" <<EOF
[Unit]
Description=Daily rotation of KARMAX logs

[Timer]
OnCalendar=daily
Persistent=true
RandomizedDelaySec=5m

[Install]
WantedBy=timers.target
EOF
else
	echo "==> logrotate not found; skipping log rotation (${DATA_DIR}/karmax.log will grow unbounded)"
fi

systemctl --user daemon-reload
[ -f "$UNIT_DIR/karmax-logrotate.timer" ] && systemctl --user enable --now karmax-logrotate.timer >/dev/null 2>&1 || true
# Keep the service alive across logout / reboot (aggressive, always-on).
loginctl enable-linger "$USER" >/dev/null 2>&1 || true
systemctl --user enable --now karmax.service

echo
echo "karmax is installed and running."
systemctl --user --no-pager status karmax.service 2>/dev/null | head -n 6 || true
echo
echo "  logs:    tail -f $DATA_DIR/karmax.log   (or: journalctl --user -u karmax.service -f)"
echo "  stop:    systemctl --user stop karmax.service"
echo "  disable: systemctl --user disable --now karmax.service"
case ":$PATH:" in
	*":$PREFIX:"*) : ;;
	*) echo "  note:  add $PREFIX to your PATH to run 'karmax' directly." ;;
esac
