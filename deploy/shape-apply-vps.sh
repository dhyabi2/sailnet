#!/bin/bash
# Like shape-apply.sh, but the measured client runs on a VPS (systemd unit
# sailclient, SAIL_HOME=/var/lib/sail-client). Relays reload on HUP; the
# client restarts with the new parameters; "flush" fetches its trace file.
#   sailtrace tune -apply deploy/shape-apply-vps.sh -socks 127.0.0.1:2080 -status 127.0.0.1:2090 ...
# with `ssh -N -L 2080:127.0.0.1:1080 -L 2090:127.0.0.1:1090 root@$CLIENT` running.
set -euo pipefail
P=${1:?params.json}; MODE=${2:-apply}
DIR=$(cd "$(dirname "$P")" && pwd)
CLIENT=${SAIL_CLIENT_HOST:-148.230.105.31}
RELAYS="${SAIL_RELAYS:-72.61.148.4 148.230.105.31 2.24.73.29}"
S="ssh -o StrictHostKeyChecking=no"
if [ "$MODE" = apply ]; then
  for ip in $RELAYS; do
    scp -q -o StrictHostKeyChecking=no "$P" root@$ip:/var/lib/sail/shape.json
    $S root@$ip 'systemctl kill -s HUP sailnode' &
  done
  wait
  scp -q -o StrictHostKeyChecking=no "$P" root@$CLIENT:/var/lib/sail-client/shape.json
  $S root@$CLIENT 'systemctl stop sailclient; rm -f /var/lib/sail-client/live.jsonl; systemctl start sailclient'
  for i in $(seq 1 60); do
    $S root@$CLIENT 'curl -s -H "X-Sail: 1" http://127.0.0.1:1090/status' 2>/dev/null | grep -q '"running":true' && exit 0
    sleep 1
  done
  echo "client did not start" >&2; exit 1
else
  $S root@$CLIENT 'systemctl stop sailclient'
  scp -q -o StrictHostKeyChecking=no root@$CLIENT:/var/lib/sail-client/live.jsonl "$DIR/live.jsonl" || true
fi
