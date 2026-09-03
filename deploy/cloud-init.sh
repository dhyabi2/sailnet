#!/bin/bash
# Sailnet relay bootstrap for a fresh Ubuntu 24.04 droplet.
# Filled in by deploy/up.sh: __CC__ __ASN__ __SEED__ __IP__ __RATE__
set -euo pipefail
export DEBIAN_FRONTEND=noninteractive
apt-get update -qq && apt-get install -y -qq curl ca-certificates >/dev/null
# Go toolchain
curl -fsSL https://go.dev/dl/go1.22.6.linux-amd64.tar.gz | tar -C /usr/local -xz
export PATH=$PATH:/usr/local/go/bin
# Source (rsynced by up.sh to /opt/sail before this runs)
cd /opt/sail && /usr/local/go/bin/go build -o /usr/local/bin/sailnode ./cmd/sailnode && /usr/local/go/bin/go build -o /usr/local/bin/sail ./cmd/sail
mkdir -p /var/lib/sail && chmod 700 /var/lib/sail
cat > /var/lib/sail/wallet.json <<EOF
{"seed":"__SEED__","index":0}
EOF
chmod 600 /var/lib/sail/wallet.json
cat > /etc/systemd/system/sailnode.service <<EOF
[Unit]
Description=Sailnet relay (accepts SAIL only)
After=network-online.target
[Service]
Environment=SAIL_HOME=/var/lib/sail
ExecStart=/usr/local/bin/sailnode relay --listen :443 --ip __IP__ --cc __CC__ --asn __ASN__ --rate __RATE__ --exit --register
Restart=always
RestartSec=5
AmbientCapabilities=CAP_NET_BIND_SERVICE
DynamicUser=no
[Install]
WantedBy=multi-user.target
EOF
systemctl daemon-reload
systemctl enable --now sailnode
ufw allow 22/tcp >/dev/null 2>&1 || true
ufw allow 443/tcp >/dev/null 2>&1 || true
echo "sailnode installed"
