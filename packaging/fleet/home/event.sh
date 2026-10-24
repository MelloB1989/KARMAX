#!/bin/sh
# Hook: one JSON line per event in ~/.claude/fleet/events.jsonl, which fleetd
# ingests and truncates (slot k of an agent: in ~/.claude/fleet/slot-k). Tool
# calls keep only the tool's name (PostToolUse adds its success; PreToolUse
# has none, it only says what is starting); StopFailure and Notification keep
# their whole payload (fleetd reads the error type from it).
d="$HOME/.claude/fleet"
[ "${FLEET_SLOT:-1}" -gt 1 ] 2>/dev/null && d="$d/slot-$FLEET_SLOT" && mkdir -p "$d"
jq -c '{t: (now|floor), event: .hook_event_name, session: .session_id}
  + (if .hook_event_name == "PostToolUse"
       then {tool: .tool_name, ok: ((.tool_response.is_error? // false) | not)}
     elif .hook_event_name == "PreToolUse"
       then {tool: .tool_name} else {} end)
  + (if .hook_event_name == "StopFailure" or .hook_event_name == "Notification"
       then {raw: .} else {} end)' >> "$d/events.jsonl" 2>/dev/null || true
exit 0
