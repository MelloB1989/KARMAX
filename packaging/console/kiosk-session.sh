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

while true; do
	# No window manager runs here, so --kiosk alone leaves Chrome at its default
	# window size: place and size the window on the whole output ourselves.
	google-chrome --kiosk --start-fullscreen --window-position=0,0 --window-size="${MODE_W},${MODE_H}" --incognito --no-first-run --noerrdialogs --disable-session-crashed-bubble \
		--ignore-gpu-blocklist \
		"http://${CONSOLE_LISTEN}/login?k=$(cat "${CONSOLE_DIR}/client.token")"
	sleep 2
done
