#!/bin/bash
# Install shaping parameters on the three live relays and restart the local
# client with them, tracing its relay connections. Used by:
#   sailtrace tune -apply deploy/shape-apply.sh -origin <ip>:8444 -dir <dir> ...
# Usage: shape-apply.sh <params.json> [flush]
set -euo pipefail
P=${1:?params.json}; MODE=${2:-apply}
DIR=$(cd "$(dirname "$P")" && pwd)
cd "$(dirname "$0")/.."
RELAYS="${SAIL_RELAYS:-72.61.148.4 148.230.105.31 2.24.73.29}"
if [ "$MODE" = apply ]; then
  for ip in $RELAYS; do
    scp -q -o StrictHostKeyChecking=no "$P" root@$ip:/var/lib/sail/shape.json
    # The service has SAIL_SHAPE=/var/lib/sail/shape.json in its environment
    # (deploy/push-code.sh sets it); HUP makes the relay reload it in place.
    ssh -o StrictHostKeyChecking=no root@$ip 'systemctl kill -s HUP sailnode' &
  done
  wait
  sleep 2
fi
# Restart the local client (a restart flushes its trace file).
pkill -f "sailnode client" || true
sleep 1
if [ "$MODE" = apply ]; then
  ( cd sail && SAIL_SHAPE="$P" SAIL_TRACE="$DIR/live.jsonl" nohup ./bin/sailnode client --socks 127.0.0.1:1080 --status 127.0.0.1:1090 --hops 3 --anchor 0.0005 --rate 0.00005 --nick Falcon >> ~/.sail/client.log 2>&1 & )
  for i in $(seq 1 90); do
    curl -s -H 'X-Sail: 1' http://127.0.0.1:1090/status 2>/dev/null | grep -q '"running":true' && exit 0
    sleep 1
  done
  echo "client did not start" >&2; exit 1
fi
