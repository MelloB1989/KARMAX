#!/usr/bin/env bash
# Runs inside X (started by fleet-kiosk.service through xinit): no blanking, no cursor,
# and Chrome on the console's login URL, restarted whenever it exits.
set -u
ENV_FILE="${FLEET_CONSOLE_ENV:?FLEET_CONSOLE_ENV is not set}"
# shellcheck disable=SC1090
. "$ENV_FILE"

# The display's clock and dates follow the operator, not the host (often UTC).
[ -n "${DISPLAY_TZ:-}" ] && export TZ="$DISPLAY_TZ"

xset s off
xset -dpms
xset s noblank
unclutter -idle 0.5 -root &
# A window manager, only so that keyboard focus follows clicks: without one, the first
# window to take focus (RustDesk's connection popup) keeps it, and no key reaches Chrome.
openbox &

while true; do
	# Place and size the window on the whole output ourselves, so Chrome fills it even if
	# the window manager is not up yet.
	google-chrome --kiosk --start-fullscreen --window-position=0,0 --window-size="${MODE_W},${MODE_H}" --incognito --no-first-run --noerrdialogs --disable-session-crashed-bubble \
		--ignore-gpu-blocklist \
		"http://${CONSOLE_LISTEN}/login?k=$(cat "${CONSOLE_DIR}/client.token")"
	sleep 2
done
