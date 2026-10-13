#!/usr/bin/env bash
# Stop the demo stack (by pid file) and wipe its state.
cd "$(dirname "$0")/.."
for f in demo/run/*.pid; do
  [ -f "$f" ] || continue
  pid=$(cat "$f")
  kill "$pid" 2>/dev/null && for _ in $(seq 1 20); do kill -0 "$pid" 2>/dev/null || break; sleep 0.25; done
  kill -9 "$pid" 2>/dev/null || true
  echo "stopped $(basename "$f" .pid)"
done
rm -rf demo/data demo/logs demo/run
echo "demo state wiped"
