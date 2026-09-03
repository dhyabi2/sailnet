#!/bin/bash
# Push the current sail/ tree to a live relay, rebuild, restart. Keeps the
# service unit and environment the box already has.
#   deploy/push-code.sh <ip>
set -euo pipefail
IP=${1:?ip}
cd "$(dirname "$0")/.."
rsync -az --delete --exclude bin --exclude '*.test' -e "ssh -o StrictHostKeyChecking=no" sail/ root@$IP:/opt/sail/
ssh -o StrictHostKeyChecking=no root@$IP 'set -e; cd /opt/sail; export GOTOOLCHAIN=auto GOFLAGS=-mod=mod; /usr/local/go/bin/go build -o /usr/local/bin/sailnode ./cmd/sailnode; /usr/local/go/bin/go build -o /usr/local/bin/sail ./cmd/sail; /usr/local/go/bin/go build -o /usr/local/bin/sailtrace ./cmd/sailtrace; grep -q SAIL_SHAPE /var/lib/sail/env || echo SAIL_SHAPE=/var/lib/sail/shape.json >> /var/lib/sail/env; [ -f /var/lib/sail/shape.json ] || echo '{}' > /var/lib/sail/shape.json; systemctl restart sailnode; sleep 2; systemctl is-active sailnode'
