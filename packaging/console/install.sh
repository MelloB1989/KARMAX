#!/usr/bin/env bash
# Installs the fleet console's PC2 host side: a virtual display, a Chrome kiosk on the
# panel, the console as a user service, and RustDesk attached to the kiosk's display.
#
#   ./install.sh [--dry-run] [--env FILE] [--only step[,step...]]
#
# Run it as the kiosk user. Steps that need root use sudo, one function each, and every
# command is printed before it runs (--dry-run prints them and runs nothing). Safe to run
# again. It prints no secret. Steps, in order:
#   check edid cmdline packages units rustdesk token
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ALL_STEPS=(check edid cmdline packages units rustdesk token)
DRY=0
ENV_FILE="$SCRIPT_DIR/console.env"
ONLY=""

# ---- command runners ------------------------------------------------------------

say() { printf '%s\n' "$*"; }

# run CMD ARGS...: print it, run it unless --dry-run.
run() {
	printf '+'
	printf ' %q' "$@"
	printf '\n'
	if ((DRY)); then return 0; fi
	"$@"
}

# run_sh 'shell text': for pipes and redirects.
run_sh() {
	printf '+ %s\n' "$1"
	if ((DRY)); then return 0; fi
	bash -c "$1"
}

as_root() { run sudo "$@"; }

# put_root FILE MODE: write stdin to FILE as root. The content is printed, so it can be audited.
put_root() {
	local file="$1" mode="$2" content
	content="$(cat)"
	printf '+ sudo install -m %s -D /dev/stdin %s   # content:\n' "$mode" "$file"
	printf '%s\n' "$content" | sed 's/^/|   /'
	if ((DRY)); then return 0; fi
	printf '%s\n' "$content" | sudo install -m "$mode" -D /dev/stdin "$file"
}

# put_user FILE MODE: the same as the kiosk user.
put_user() {
	local file="$1" mode="$2" content
	content="$(cat)"
	printf '+ install -m %s -D /dev/stdin %s   # content:\n' "$mode" "$file"
	printf '%s\n' "$content" | sed 's/^/|   /'
	if ((DRY)); then return 0; fi
	printf '%s\n' "$content" | install -m "$mode" -D /dev/stdin "$file"
}

die() {
	printf 'install.sh: %s\n' "$*" >&2
	exit 1
}

# ---- config ---------------------------------------------------------------------

load_env() {
	[[ -r "$ENV_FILE" ]] || die "cannot read $ENV_FILE (copy console.env.example and edit it)"
	# shellcheck disable=SC1090
	. "$ENV_FILE"
	local v
	for v in KIOSK_USER CONSOLE_DIR CONSOLE_BIN CONSOLE_LISTEN CONNECTOR MODE_W MODE_H MODE_HZ \
		CHROME_PKG CHROME_APT_URL CHROME_KEY_URL RUSTDESK_VERSION RUSTDESK_URL RUSTDESK_SHA256 \
		RUSTDESK_MODE RUSTDESK_DIRECT_PORT; do
		[[ -n "${!v:-}" ]] || die "$v is empty in $ENV_FILE"
	done
	case "$RUSTDESK_MODE" in public | direct) ;; *) die "RUSTDESK_MODE must be public or direct" ;; esac
	EDID_FILE="fleet-${MODE_W}x${MODE_H}.bin"
	KIOSK_HOME="$(getent passwd "$KIOSK_USER" | cut -d: -f6 || true)"
	: "${KIOSK_HOME:=/home/$KIOSK_USER}"
}

# edid_param: drm.edid_firmware, or drm_kms_helper.edid_firmware on kernels that only have that one.
edid_param() {
	if [[ -n "${EDID_PARAM:-}" ]]; then
		printf '%s' "$EDID_PARAM"
	elif command -v modinfo >/dev/null 2>&1 && ! modinfo -p drm 2>/dev/null | grep -q '^edid_firmware' &&
		modinfo -p drm_kms_helper 2>/dev/null | grep -q '^edid_firmware'; then
		printf 'drm_kms_helper.edid_firmware'
	else
		printf 'drm.edid_firmware'
	fi
}

# cmdline_params: what the kernel command line gains.
cmdline_params() {
	printf 'video=%s:%sx%s@%se %s=%s:edid/%s' "$CONNECTOR" "$MODE_W" "$MODE_H" "$MODE_HZ" \
		"$(edid_param)" "$CONNECTOR" "$EDID_FILE"
}

# grub_apply PARAMS MARKER: reads /etc/default/grub on stdin, writes it with PARAMS added to
# GRUB_CMDLINE_LINUX_DEFAULT once (a line already containing MARKER is left alone).
grub_apply() {
	local params="$1" marker="$2"
	awk -v params="$params" -v marker="$marker" '
		/^GRUB_CMDLINE_LINUX_DEFAULT=/ {
			seen = 1
			if (index($0, marker) == 0) {
				if ($0 !~ /="[^"]*"[[:space:]]*$/) { print "GRUB_CMDLINE_LINUX_DEFAULT is not double-quoted" > "/dev/stderr"; exit 2 }
				sub(/"[[:space:]]*$/, "")
				sep = ($0 ~ /="$/) ? "" : " "
				$0 = $0 sep params "\""
			}
		}
		{ print }
		END { if (!seen) print "GRUB_CMDLINE_LINUX_DEFAULT=\"" params "\"" }
	'
}

xorg_output() { # the kernel's HDMI-A-1 is Xorg's HDMI-1
	printf '%s' "$CONNECTOR" | sed -E 's/-A-/-/'
}

# render FILE: fill the @...@ placeholders of a unit file.
render() {
	sed -e "s|@KIOSK_USER@|$KIOSK_USER|g" \
		-e "s|@CONSOLE_DIR@|$CONSOLE_DIR|g" \
		-e "s|@CONSOLE_BIN@|$CONSOLE_BIN|g" \
		-e "s|@CONSOLE_ENV@|$CONSOLE_DIR/console.env|g" \
		-e "s|@KIOSK_SESSION@|$CONSOLE_DIR/kiosk-session.sh|g" "$1"
}

# ---- steps ----------------------------------------------------------------------

step_check() {
	say "== check: the connector, the render node, the groups"
	if ((DRY)); then
		say "+ test -e /sys/class/drm/card*-$CONNECTOR/status"
		say "+ ls /dev/dri/renderD*"
		say "+ id -nG $KIOSK_USER | grep -w video; id -nG $KIOSK_USER | grep -w render"
		return 0
	fi
	[[ "$(id -un)" == "$KIOSK_USER" ]] || die "run this as $KIOSK_USER (it is $(id -un))"
	local st
	st="$(compgen -G "/sys/class/drm/card*-$CONNECTOR/status" | head -n1 || true)"
	[[ -n "$st" ]] || die "no /sys/class/drm/card*-$CONNECTOR/status: set CONNECTOR to a connector this host has"
	say "connector $CONNECTOR is $(cat "$st")"
	compgen -G '/dev/dri/renderD*' >/dev/null || die "no /dev/dri/renderD*: no GPU render node"
	local g
	for g in video render; do
		id -nG "$KIOSK_USER" | tr ' ' '\n' | grep -qx "$g" || die "$KIOSK_USER is not in the $g group: sudo usermod -aG $g $KIOSK_USER"
	done
}

step_edid() {
	say "== edid: $EDID_FILE and the initramfs hook that copies it"
	local tmp="${TMPDIR:-/tmp}/$EDID_FILE"
	run_sh "python3 $SCRIPT_DIR/edid/make-edid.py $MODE_W $MODE_H $MODE_HZ > $tmp"
	as_root install -m 0644 -D "$tmp" "/lib/firmware/edid/$EDID_FILE"
	put_root /etc/initramfs-tools/hooks/fleet-edid 0755 <<EOF
#!/bin/sh
# installed by the fleet console: puts the virtual display's EDID in the initramfs
PREREQ=""
prereqs() { echo "\$PREREQ"; }
case "\$1" in prereqs) prereqs; exit 0 ;; esac
. /usr/share/initramfs-tools/hook-functions
mkdir -p "\${DESTDIR}/lib/firmware/edid"
cp /lib/firmware/edid/$EDID_FILE "\${DESTDIR}/lib/firmware/edid/"
EOF
}

step_cmdline() {
	say "== cmdline: the kernel gains a virtual output (needs a reboot)"
	local params bak=/etc/default/grub.fleet-console.bak
	params="$(cmdline_params)"
	say "GRUB line: GRUB_CMDLINE_LINUX_DEFAULT gains: $params"
	# the first backup is the original; later runs never overwrite it
	run_sh "test -e $bak || sudo cp -p /etc/default/grub $bak"
	if ((DRY)); then
		say "+ grub_apply '$params' 'video=$CONNECTOR:' < /etc/default/grub > /etc/default/grub.new"
		say "+ sudo install -m 0644 /etc/default/grub.new /etc/default/grub   # only if it differs"
	else
		local new
		new="$(mktemp)"
		grub_apply "$params" "video=$CONNECTOR:" </etc/default/grub >"$new" || die "could not edit /etc/default/grub"
		if cmp -s "$new" /etc/default/grub; then
			say "already present: nothing to change"
		else
			as_root install -m 0644 "$new" /etc/default/grub
		fi
		rm -f "$new"
	fi
	as_root update-grub
	as_root update-initramfs -u
	say "UNDO: sudo cp -p $bak /etc/default/grub && sudo update-grub && sudo update-initramfs -u"
	say "      or at the GRUB menu press e, delete '$params' from the linux line, and boot with Ctrl+x."
	say "REBOOT REQUIRED for the virtual output. SSH is unaffected either way."
}

step_packages() {
	say "== packages: Xorg, Chrome from its apt repo, RustDesk's pinned .deb"
	as_root apt-get update
	as_root apt-get install -y xserver-xorg-core xinit x11-xserver-utils unclutter curl gpg ca-certificates
	run_sh "curl -fsSL '$CHROME_KEY_URL' | gpg --dearmor | sudo tee /usr/share/keyrings/google-chrome.gpg >/dev/null"
	put_root /etc/apt/sources.list.d/google-chrome.list 0644 <<EOF
deb [arch=amd64 signed-by=/usr/share/keyrings/google-chrome.gpg] $CHROME_APT_URL stable main
EOF
	as_root apt-get update
	as_root apt-get install -y "$CHROME_PKG"
	local work deb
	if ((DRY)); then work="<tmpdir>"; else work="$(mktemp -d)"; fi
	deb="$work/rustdesk-$RUSTDESK_VERSION.deb"
	run curl -fsSL -o "$deb" "$RUSTDESK_URL"
	run_sh "echo '$RUSTDESK_SHA256  $deb' | sha256sum -c -"
	as_root apt-get install -y "$deb"
}

step_units() {
	say "== units: Xorg, the console user service, the kiosk service"
	put_root /etc/X11/Xwrapper.config 0644 <<'EOF'
allowed_users=anybody
needs_root_rights=yes
EOF
	put_root /etc/X11/xorg.conf.d/20-fleet-display.conf 0644 <<EOF
Section "Device"
    Identifier "fleet-gpu"
    Driver "modesetting"
    Option "Monitor-$(xorg_output)" "fleet-virtual"
EndSection

Section "Monitor"
    Identifier "fleet-virtual"
    Option "PreferredMode" "${MODE_W}x${MODE_H}"
    Option "DPMS" "false"
EndSection
EOF
	run install -d -m 0700 "$CONSOLE_DIR"
	run install -m 0600 "$ENV_FILE" "$CONSOLE_DIR/console.env"
	run install -m 0755 "$SCRIPT_DIR/kiosk-session.sh" "$CONSOLE_DIR/kiosk-session.sh"
	render "$SCRIPT_DIR/fleet-console.service" | put_user "$KIOSK_HOME/.config/systemd/user/fleet-console.service" 0644
	as_root loginctl enable-linger "$KIOSK_USER"
	run systemctl --user daemon-reload
	run systemctl --user enable --now fleet-console.service
	render "$SCRIPT_DIR/fleet-kiosk.service" | put_root /etc/systemd/system/fleet-kiosk.service 0644
	as_root systemctl daemon-reload
	as_root systemctl set-default graphical.target
	as_root systemctl enable fleet-kiosk.service
}

step_rustdesk() {
	say "== rustdesk: the service, attached to the kiosk's display :0"
	put_root /etc/systemd/system/rustdesk.service.d/fleet.conf 0644 <<EOF
[Service]
Environment=DISPLAY=:0
Environment=XAUTHORITY=$KIOSK_HOME/.Xauthority
EOF
	as_root systemctl daemon-reload
	as_root systemctl enable --now rustdesk.service
	if [[ "$RUSTDESK_MODE" == direct ]]; then
		say "RUSTDESK_MODE=direct: direct IP access only"
		as_root rustdesk --option direct-server Y
		as_root rustdesk --option direct-access-port "$RUSTDESK_DIRECT_PORT"
	fi
	say "NEXT (operator, by hand): sudo rustdesk --password <a password you choose>"
}

step_token() {
	say "== token: $CONSOLE_DIR/client.token (generated once, never printed)"
	local f="$CONSOLE_DIR/client.token"
	if ((DRY)); then
		say "+ (umask 077; mkdir -p $CONSOLE_DIR; test -s $f || head -c32 /dev/urandom | od -An -tx1 | tr -d ' \\n' > $f)"
		return 0
	fi
	if [[ -s "$f" ]]; then
		say "exists: left alone"
		return 0
	fi
	(umask 077 && mkdir -p "$CONSOLE_DIR" && head -c32 /dev/urandom | od -An -tx1 | tr -d ' \n' >"$f")
	chmod 600 "$f"
	say "created (0600)"
}

# ---- main -----------------------------------------------------------------------

usage() { sed -n '2,10p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'; }

main() {
	while (($#)); do
		case "$1" in
		--dry-run) DRY=1 ;;
		--env)
			ENV_FILE="${2:?--env needs a file}"
			shift
			;;
		--only)
			ONLY="${2:?--only needs a step list}"
			shift
			;;
		-h | --help)
			usage
			exit 0
			;;
		*) die "unknown argument $1" ;;
		esac
		shift
	done
	load_env
	local steps=("${ALL_STEPS[@]}") s
	if [[ -n "$ONLY" ]]; then IFS=, read -r -a steps <<<"$ONLY"; fi
	if ((DRY)); then say "# dry run: nothing below is executed"; fi
	for s in "${steps[@]}"; do
		declare -F "step_$s" >/dev/null || die "unknown step $s"
		"step_$s"
	done
}

if [[ "${BASH_SOURCE[0]}" == "$0" ]]; then main "$@"; fi
