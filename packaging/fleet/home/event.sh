#!/bin/sh
# Hook: one JSON line per event in ~/.claude/fleet/events.jsonl, which fleetd
# ingests and truncates. Tool calls keep only the tool's name and success;
# StopFailure and Notification keep their whole payload (fleetd reads the
# error type from it).
jq -c '{t: (now|floor), event: .hook_event_name, session: .session_id}
  + (if .hook_event_name == "PostToolUse"
       then {tool: .tool_name, ok: ((.tool_response.is_error? // false) | not)} else {} end)
  + (if .hook_event_name == "StopFailure" or .hook_event_name == "Notification"
       then {raw: .} else {} end)' >> "$HOME/.claude/fleet/events.jsonl" 2>/dev/null || true
exit 0
