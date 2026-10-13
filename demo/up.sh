#!/usr/bin/env bash
# Start the whole demo stack locally. Safe to run from anywhere.
set -euo pipefail
cd "$(dirname "$0")/.."
mkdir -p demo/run demo/logs demo/data
export PATH=$PATH:/usr/local/go/bin:$HOME/go/bin

# Ports/URLs and the config can be overridden from env (a second stack on another port offset):
#   FAKE_GITHUB CHAT_URL PAYMENTS_URL KARMAX_URL KARMAX_CONFIG   (defaults are the canonical ports)
FAKE_GITHUB=${FAKE_GITHUB:-http://127.0.0.1:8787}; CHAT_URL=${CHAT_URL:-http://127.0.0.1:9393}
PAYMENTS_URL=${PAYMENTS_URL:-http://127.0.0.1:9494}; KARMAX_URL=${KARMAX_URL:-http://127.0.0.1:9090}
KARMAX_CONFIG=${KARMAX_CONFIG:-demo/karmax.yaml}
export FAKE_GITHUB CHAT_URL PAYMENTS_URL KARMAX_URL
hp() { local u=${1#*://}; echo "${u%%/*}"; }   # URL -> host:port
GH_HP=$(hp "$FAKE_GITHUB"); CHAT_HP=$(hp "$CHAT_URL"); PAY_HP=$(hp "$PAYMENTS_URL"); KMX_HP=$(hp "$KARMAX_URL")

wait_port() { # name host:port
  for _ in $(seq 1 60); do
    (exec 3<>"/dev/tcp/${2%:*}/${2#*:}") 2>/dev/null && { echo "  ✓ $1 up on $2"; return 0; }
    sleep 0.5
  done
  echo "  ✗ $1 did not open $2 — see demo/logs/" >&2; return 1
}
start() { # name cmd...
  local name=$1; shift
  nohup "$@" >"demo/logs/$name.log" 2>&1 &
  echo $! >"demo/run/$name.pid"
}

# Always build: Go's cache makes it quick, and a `git pull` is then the whole deploy.
echo "building (cached)…"
go build -o bin/fakegithub ./cmd/fakegithub
CGO_ENABLED=1 go build -o bin/karmax ./cmd/karmax   # SQLite needs cgo (and gcc)
go build -o bin/payments ./demo/payments
chmod +x demo/*.sh

# Run copies under distinct names so another task's `pkill -x karmax` cannot hit this stack.
cp -f bin/fakegithub bin/a-fakegithub; cp -f bin/karmax bin/a-karmax

start fakegithub bin/a-fakegithub -addr "$GH_HP" -webhook "$KARMAX_URL/orgops/github" -secret dev -seed acme
wait_port "fake GitHub" "$GH_HP"

if [ -f demo/chat/server.py ]; then
  CHAT_ADDR=$CHAT_HP KARMAX_COMMS_URL="$KARMAX_URL/comms/chat?token=demo-token" start chat python3 demo/chat/server.py
  wait_port "chat" "$CHAT_HP"
else
  echo "  ! demo/chat/server.py not present yet (task B) — skipping chat; scripts fall back to KARMAX logs" >&2
fi

start payments bin/payments -addr "$PAY_HP"
wait_port "payments" "$PAY_HP"

# Model auth for the harness (token lives outside the repo; never printed).
[ -f ~/.config/karmax-demo/env ] && { set -a; . ~/.config/karmax-demo/env; set +a; }
export PATH="$PWD/bin:$PATH"   # the agent's session calls back into KARMAX with the `karmax` CLI
start karmax bin/a-karmax start -c "$KARMAX_CONFIG"
wait_port "KARMAX webhooks" "$KMX_HP"

# Make the org look real: more repositories (one deliberately out of standard).
echo "seeding the org…"
demo/act.sh seed >/dev/null && echo "  ✓ org seeded" || echo "  ! seed failed (run demo/act.sh seed)" >&2

cat <<EOT

Stack is up.
  chat page        $CHAT_URL
  fake GitHub      $FAKE_GITHUB/_fake/state
  KARMAX webhooks  $KARMAX_URL
  payments         $PAYMENTS_URL/health
  logs             demo/logs/*.log
Next: demo/scene1.sh, demo/scene2.sh   (reset: demo/down.sh)
EOT
