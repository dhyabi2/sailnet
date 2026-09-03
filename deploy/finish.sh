#!/bin/bash
# After the relay wallets are funded: receive XNO, then run the installer on each droplet.
set -euo pipefail
cd "$(dirname "$0")/.."
while read -r name ip addr; do
  echo "== $name ($ip) $addr"
  ssh -o StrictHostKeyChecking=no root@$ip 'bash /root/init.sh && sleep 3 && SAIL_HOME=/var/lib/sail sail receive && systemctl restart sailnode && sleep 2 && journalctl -u sailnode -n 5 --no-pager'
done < deploy/state/relays.txt
echo "relays on ledger:"
(cd sail && go run ./cmd/sailnode relays)
