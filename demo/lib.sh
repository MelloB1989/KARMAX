# Shared helpers for scene scripts (sourced).
GH=${FAKE_GITHUB:-http://127.0.0.1:8787}
CHAT=${CHAT_URL:-http://127.0.0.1:9393}
PAY=${PAYMENTS_URL:-http://127.0.0.1:9494}
KMX=${KARMAX_URL:-http://127.0.0.1:9090}
REPO=acme/payments-api

say() { printf '\n\033[1;36m▶ %s\033[0m\n' "$*"; }
chat_up() { curl -fsS -m 2 "$CHAT/messages?since=0" >/dev/null 2>&1; }
messages() { curl -fsS -m 3 "$CHAT/messages?since=${1:-0}"; }
# wait_msg <jq-select-on-.text regex> [timeout-seconds] -> prints first matching message text
wait_msg() {
  local re=$1 t=${2:-90} i
  for i in $(seq 1 "$t"); do
    if chat_up; then
      local m; m=$(messages 0 | jq -r --arg re "$re" '[.[]|select(.from=="karmax" and (.text|test($re)))][0].text // empty')
      [ -n "$m" ] && { printf '%s' "$m"; return 0; }
    fi
    sleep 1
  done
  return 1
}
pr_field() { curl -fsS "$GH/_fake/state" | jq -r "$1"; }
open_pr() { curl -fsS -XPOST "$GH/_fake/pulls" -d "$1" | jq -r .number; }
set_ci() { curl -fsS -XPOST "$GH/_fake/ci" -d "{\"repo\":\"$REPO\",\"number\":$1,\"state\":\"$2\"}" >/dev/null; }
summary() {
  curl -fsS "$GH/_fake/state" | jq '{prs:[.prs[]|{number,title,state,merged}],
    reviews:[(.reviews//[])[]|{number,event,inline_comments:(.comments|length)}], merges:.merges}'
}
