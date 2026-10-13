#!/usr/bin/env bash
# Scene 2 — incident: payments breaks, KARMAX alerts, the presenter approves, a fix PR ships, the incident closes.
#   scene2.sh [--auto-approve]
set -euo pipefail
cd "$(dirname "$0")/.."
. demo/lib.sh
AUTO=0; [ "${1:-}" = "--auto-approve" ] && AUTO=1

say "Breaking payments (fault on)"
curl -fsS -XPOST "$PAY/fault/on"; echo
echo "health now: $(curl -s -o /dev/null -w '%{http_code}' "$PAY/health")"

say "Waiting for KARMAX to notice (probe every 5s, window 4)…"
if chat_up; then
  msg=$(wait_msg '🚨' 120) || { echo "no 🚨 message within 120s — check demo/logs/karmax.log" >&2; exit 1; }
  printf '%s\n' "$msg"
  say "Incident opened — KARMAX is asking for approval to send a fix agent…"
  amsg=$(wait_msg 'approve [A-Z0-9]{5}' 60) || { echo "no approval request within 60s" >&2; exit 1; }
  code=$(printf '%s' "$amsg" | grep -oE 'approve [A-Z0-9]{5}' | head -1 | awk '{print $2}')
else
  echo "(chat server not running — reading KARMAX logs)"
  for _ in $(seq 1 120); do
    code=$(grep -hoE 'approve [A-Z0-9]{5}' demo/logs/karmax.log 2>/dev/null | head -1 | awk '{print $2}')
    [ -n "$code" ] && break; sleep 1
  done
fi
[ -n "${code:-}" ] || { echo "no approve code found" >&2; exit 1; }

printf '\n\033[1;33m'
echo "  ╔════════════════════════════════════╗"
printf "  ║   TYPE IN CHAT:  approve %-9s ║\n" "$code"
echo "  ╚════════════════════════════════════╝"
printf '\033[0m\n'

if [ $AUTO = 1 ]; then
  say "--auto-approve: posting approve $code as Maya"
  curl -fsS -XPOST "$KMX/comms/chat?token=demo-token" \
    -d "{\"text\":\"approve $code\",\"user\":\"Maya\",\"channel\":\"eng\"}"; echo
fi

say "Following progress until the incident closes…"
seen=$(messages 0 | jq "map(.id)|max // 0")
for _ in $(seq 1 240); do
  if chat_up; then
    messages "$seen" | jq -r '.[]|select(.from=="karmax")|"  [karmax] "+(.text|split("\n")[0])' | tail -n +1
    seen=$(messages 0 | jq 'map(.id)|max // 0')
    messages 0 | jq -e '[.[]|select(.from=="karmax" and (.text|test("resolved|closed|✅.*incident|recovered";"i")))]|length>0' >/dev/null && break
  else
    grep -qiE 'incident.*(resolved|closed)' demo/logs/karmax.log && break
  fi
  sleep 3
done
say "Final state"
echo "payments health: $(curl -s -o /dev/null -w '%{http_code}' "$PAY/health")"
summary
