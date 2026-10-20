#!/usr/bin/env bash
# Tests host-bootstrap.sh without root: the Docker drop-in that waits for the code mount.
#   packaging/fleet/test-bootstrap.sh
set -uo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
FAILS=0
ok() { printf 'ok   %s\n' "$1"; }
fail() {
	printf 'FAIL %s\n' "$1"
	FAILS=$((FAILS + 1))
}
has() { if grep -qxF -- "$3" <<<"$2"; then ok "$1"; else fail "$1 (missing line: $3)"; fi; }

# The real systemd-escape is used when this machine has one (PC2 does). The agent image does not, so
# otherwise a stand-in with the same output for plain paths keeps the wiring under test.
if command -v systemd-escape >/dev/null 2>&1; then
	printf 'using the real systemd-escape\n'
else
	SHIM="$(mktemp -d)"
	trap 'rm -rf "$SHIM"' EXIT
	cat >"$SHIM/systemd-escape" <<'SH'
#!/usr/bin/env bash
# stand-in: systemd-escape --path --suffix=mount PATH, for absolute paths of [A-Za-z0-9_/.-]
p="${3#/}"; p="${p%/}"; p="${p//-/\\x2d}"; echo "${p//\//-}.${2#--suffix=}"
SH
	chmod +x "$SHIM/systemd-escape"
	PATH="$SHIM:$PATH"
	printf 'no systemd-escape here: using a stand-in\n'
fi

OUT="$("$HERE/host-bootstrap.sh" docker-dropin --code /srv/x/code 2>&1)"
has "waits for the mount" "$OUT" "After=srv-x-code.mount network-online.target"
has "and wants it" "$OUT" "Wants=srv-x-code.mount network-online.target"
has "is a [Unit] section" "$OUT" "[Unit]"

# the unit name follows the path, escaping included
OUT="$("$HERE/host-bootstrap.sh" docker-dropin --code /srv/my-code 2>&1)"
has "dashes are escaped" "$OUT" 'After=srv-my\x2dcode.mount network-online.target'

if "$HERE/host-bootstrap.sh" docker-dropin >/dev/null 2>&1; then fail "needs --code"; else ok "needs --code"; fi
if [ "$(id -u)" -ne 0 ]; then
	if "$HERE/host-bootstrap.sh" pc2 --kali-ip 192.0.2.1 --code /srv/x/code --pubkey k >/dev/null 2>&1; then
		fail "the pc2 mode still requires root"
	else ok "the pc2 mode still requires root"; fi
fi

# the pc2 mode writes that drop-in where Docker reads it
if grep -q 'docker_mount_dropin "\$code" > /etc/systemd/system/docker.service.d/fleet-code-mount.conf' "$HERE/host-bootstrap.sh"; then
	ok "pc2 installs the drop-in"
else fail "pc2 installs the drop-in"; fi

# the code mount never stops retrying, and pc2 installs that beside the mount unit
OUT="$("$HERE/host-bootstrap.sh" mount-dropin 2>&1)"
has "the mount has no start limit" "$OUT" "StartLimitIntervalSec=0"
has "in its [Unit] section" "$OUT" "[Unit]"
if grep -qF 'mount_retry_dropin > "/etc/systemd/system/$mount_unit.d/fleet-retry.conf"' "$HERE/host-bootstrap.sh"; then
	ok "pc2 installs the retry drop-in"
else fail "pc2 installs the retry drop-in"; fi
if bash -n "$HERE/host-bootstrap.sh"; then ok "bash -n"; else fail "bash -n"; fi
printf '\n%d failed\n' "$FAILS"
[ "$FAILS" -eq 0 ]
