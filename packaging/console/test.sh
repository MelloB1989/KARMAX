#!/usr/bin/env bash
# Tests for install.sh without root: the dry run proves every command, grub_apply is
# idempotent, the EDID generator is checked, and the scripts parse (and lint, if shellcheck is here).
#   packaging/console/test.sh
set -uo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
FAILS=0

ok() { printf 'ok   %s\n' "$1"; }
fail() {
	printf 'FAIL %s\n' "$1"
	FAILS=$((FAILS + 1))
}
# expect NAME TEXT PATTERN: TEXT contains the fixed string PATTERN
expect() { if grep -qF -- "$3" <<<"$2"; then ok "$1"; else fail "$1 (missing: $3)"; fi; }
refuse() { if grep -qF -- "$3" <<<"$2"; then fail "$1 (found: $3)"; else ok "$1"; fi; }

cat >"$TMP/console.env" <<EOF
KIOSK_USER=kiosktest
CONSOLE_DIR=$TMP/console-dir
CONSOLE_BIN=/opt/test/fleet-console
CONSOLE_LISTEN=127.0.0.1:7880
CONNECTOR=HDMI-A-1
MODE_W=1920
MODE_H=1080
MODE_HZ=60
EDID_PARAM=drm.edid_firmware
CHROME_PKG=google-chrome-stable
CHROME_APT_URL=https://apt.example.invalid/chrome/
CHROME_KEY_URL=https://apt.example.invalid/key.pub
RUSTDESK_VERSION=9.9.9
RUSTDESK_URL=https://dl.example.invalid/rustdesk-9.9.9.deb
RUSTDESK_SHA256=0123456789abcdef
RUSTDESK_MODE=direct
RUSTDESK_DIRECT_PORT=21118
EOF

# ---- the dry run ----------------------------------------------------------------
OUT="$("$HERE/install.sh" --dry-run --env "$TMP/console.env" 2>&1)"
rc=$?
[[ $rc -eq 0 ]] && ok "dry run exits 0" || fail "dry run exits $rc"

expect "GRUB line" "$OUT" "video=HDMI-A-1:1920x1080@60e drm.edid_firmware=HDMI-A-1:edid/fleet-1920x1080.bin"
expect "undo line" "$OUT" "UNDO: sudo cp -p /etc/default/grub.fleet-console.bak /etc/default/grub && sudo update-grub && sudo update-initramfs -u"
expect "reboot notice" "$OUT" "REBOOT REQUIRED"
expect "grub backup is kept once" "$OUT" "test -e /etc/default/grub.fleet-console.bak || sudo cp -p /etc/default/grub"
expect "update-grub" "$OUT" "+ sudo update-grub"
expect "update-initramfs" "$OUT" "+ sudo update-initramfs -u"
expect "edid generated" "$OUT" "make-edid.py 1920 1080 60"
expect "edid installed" "$OUT" "/lib/firmware/edid/fleet-1920x1080.bin"
expect "initramfs hook" "$OUT" "/etc/initramfs-tools/hooks/fleet-edid"
expect "connector check" "$OUT" "/sys/class/drm/card*-HDMI-A-1/status"
expect "render node check" "$OUT" "/dev/dri/renderD*"
expect "chrome apt repo" "$OUT" "deb [arch=amd64 signed-by=/usr/share/keyrings/google-chrome.gpg] https://apt.example.invalid/chrome/ stable main"
expect "rustdesk checksum" "$OUT" "0123456789abcdef"
expect "rustdesk checksum is verified" "$OUT" "sha256sum -c -"
expect "console user service" "$OUT" "ExecStart=/opt/test/fleet-console --config $TMP/console-dir/console.yaml"
expect "kiosk system service" "$OUT" "/etc/systemd/system/fleet-kiosk.service"
expect "kiosk service runs xinit" "$OUT" "ExecStart=/usr/bin/xinit $TMP/console-dir/kiosk-session.sh -- :0 vt1"
expect "lingering" "$OUT" "loginctl enable-linger kiosktest"
expect "rustdesk on :0" "$OUT" "Environment=DISPLAY=:0"
expect "rustdesk direct mode" "$OUT" "direct-server Y"
expect "token step" "$OUT" "client.token"
expect "operator sets the password" "$OUT" "sudo rustdesk --password"
refuse "no unrendered placeholder" "$OUT" "@KIOSK_USER@"
refuse "no unrendered placeholder (dir)" "$OUT" "@CONSOLE_DIR@"

if [[ -e "$TMP/console-dir" ]]; then fail "dry run created files"; else ok "dry run changed nothing"; fi

# every command line of the dry run starts with a printed '+' (nothing runs unannounced)
# and the public-mode run skips the direct-access options
PUB="$(sed 's/RUSTDESK_MODE=direct/RUSTDESK_MODE=public/' "$TMP/console.env")"
printf '%s\n' "$PUB" >"$TMP/public.env"
OUT_PUB="$("$HERE/install.sh" --dry-run --env "$TMP/public.env" --only rustdesk 2>&1)"
refuse "public mode sets no direct options" "$OUT_PUB" "direct-server"

OUT_ONE="$("$HERE/install.sh" --dry-run --env "$TMP/console.env" --only token 2>&1)"
refuse "--only runs only that step" "$OUT_ONE" "update-grub"

if "$HERE/install.sh" --dry-run --env "$TMP/console.env" --only nope >/dev/null 2>&1; then fail "unknown step rejected"; else ok "unknown step rejected"; fi

# ---- the GRUB edit ----------------------------------------------------------------
# shellcheck disable=SC1091
source "$HERE/install.sh"
P="video=HDMI-A-1:1920x1080@60e drm.edid_firmware=HDMI-A-1:edid/fleet-1920x1080.bin"
cat >"$TMP/grub" <<'EOF'
GRUB_DEFAULT=0
GRUB_CMDLINE_LINUX_DEFAULT="quiet splash"
GRUB_CMDLINE_LINUX=""
EOF
grub_apply "$P" "video=HDMI-A-1:" <"$TMP/grub" >"$TMP/grub1"
expect "grub: params appended" "$(cat "$TMP/grub1")" "GRUB_CMDLINE_LINUX_DEFAULT=\"quiet splash $P\""
grub_apply "$P" "video=HDMI-A-1:" <"$TMP/grub1" >"$TMP/grub2"
if cmp -s "$TMP/grub1" "$TMP/grub2"; then ok "grub: a second run changes nothing"; else fail "grub: a second run changed the file"; fi
expect "grub: other lines kept" "$(cat "$TMP/grub1")" "GRUB_CMDLINE_LINUX=\"\""
printf 'GRUB_CMDLINE_LINUX_DEFAULT=""\n' | grub_apply "$P" "video=HDMI-A-1:" >"$TMP/grub3"
expect "grub: an empty line gets no leading space" "$(cat "$TMP/grub3")" "GRUB_CMDLINE_LINUX_DEFAULT=\"$P\""
printf 'GRUB_DEFAULT=0\n' | grub_apply "$P" "video=HDMI-A-1:" >"$TMP/grub4"
expect "grub: a missing line is added" "$(cat "$TMP/grub4")" "GRUB_CMDLINE_LINUX_DEFAULT=\"$P\""

# ---- the EDID generator ---------------------------------------------------------
if python3 -m unittest discover -s "$HERE/edid" -p 'test_*.py' >"$TMP/py.out" 2>&1; then
	ok "edid: python tests"
else
	fail "edid: python tests"
	cat "$TMP/py.out"
fi

# ---- syntax and lint ------------------------------------------------------------
for f in install.sh kiosk-session.sh test.sh; do
	if bash -n "$HERE/$f"; then ok "bash -n $f"; else fail "bash -n $f"; fi
done
if command -v shellcheck >/dev/null 2>&1; then
	if shellcheck -x "$HERE/install.sh" "$HERE/kiosk-session.sh" "$HERE/test.sh"; then ok "shellcheck"; else fail "shellcheck"; fi
else
	printf 'skip shellcheck (not installed here; run it where it is)\n'
fi

printf '\n%d failed\n' "$FAILS"
[[ $FAILS -eq 0 ]]
