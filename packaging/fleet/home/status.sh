#!/bin/sh
# Status line: keep the latest snapshot for fleetd (the account's 5h/7d
# windows, context, cost), atomically, and print a short line for the pane.
in=$(cat)
d="$HOME/.claude/fleet"
printf '%s\n' "$in" > "$d/status.json.tmp" && mv "$d/status.json.tmp" "$d/status.json"
printf '%s\n' "$in" | jq -r '"\(env.FLEET_AGENT // "agent") · \(.model.display_name // "?") · ctx \(.context_window.used_percentage // 0 | floor)% · 5h \(.rate_limits.five_hour.used_percentage // "-")% · 7d \(.rate_limits.seven_day.used_percentage // "-")%"' 2>/dev/null || true
