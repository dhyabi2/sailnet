#!/bin/bash
# Install a pruned Nano node on Ubuntu/Debian as the local RPC for a Sailnet relay.
#   deploy/nano-node.sh [version]
# Prerequisite for running a live relay or home node: the relay must not disclose
# its payers, tags and peers to a public RPC provider, must verify confirmations
# itself, and must not depend on a provider's rate limits.
#
# Sizing: 4 GB RAM minimum (8 GB recommended), SSD with at least 60 GB free for a
# pruned ledger, one dedicated core. Initial sync takes hours.
set -euo pipefail
VER=${1:-V28.0}
NANO_DIR=/var/lib/nano
FREE_GB=$(df -BG --output=avail / | tail -1 | tr -dc '0-9')
if [ "$FREE_GB" -lt 60 ]; then
  echo "need at least 60 GB free on / for a pruned ledger (have ${FREE_GB} GB); resize the disk first" >&2
  exit 1
fi
MEM_MB=$(free -m | awk '/^Mem:/{print $2}')
if [ "$MEM_MB" -lt 3800 ]; then
  echo "need at least 4 GB RAM (have ${MEM_MB} MB)" >&2
  exit 1
fi
export DEBIAN_FRONTEND=noninteractive
apt-get update -qq >/dev/null && apt-get install -y -qq curl ca-certificates >/dev/null
id -u nano >/dev/null 2>&1 || useradd -r -m -d "$NANO_DIR" -s /usr/sbin/nologin nano
mkdir -p "$NANO_DIR" && chown nano:nano "$NANO_DIR"
if [ ! -x /usr/local/bin/nano_node ]; then
  URL="https://github.com/nanocurrency/nano-node/releases/download/${VER}/nano-node-${VER}-Linux.tar.bz2"
  echo "downloading $URL"
  curl -fsSL "$URL" -o /tmp/nano-node.tar.bz2
  mkdir -p /tmp/nano-node && tar -xjf /tmp/nano-node.tar.bz2 -C /tmp/nano-node --strip-components=1
  install -m 755 /tmp/nano-node/bin/nano_node /usr/local/bin/nano_node
  rm -rf /tmp/nano-node /tmp/nano-node.tar.bz2
fi
# Node config: pruning on, RPC on loopback only, no control commands.
sudo -u nano /usr/local/bin/nano_node --data_path "$NANO_DIR" --generate_config node > "$NANO_DIR/config-node.toml.default" 2>/dev/null || true
cat > "$NANO_DIR/config-node.toml" <<'EOF'
[node]
enable_pruning = true
enable_voting = false
[rpc]
enable = true
enable_sign_hash = false
EOF
cat > "$NANO_DIR/config-rpc.toml" <<'EOF'
address = "::ffff:127.0.0.1"
port = 7076
enable_control = false
max_json_depth = 20
EOF
chown -R nano:nano "$NANO_DIR"
cat > /etc/systemd/system/nano-node.service <<EOF
[Unit]
Description=Nano node (pruned) for Sailnet
After=network-online.target
[Service]
User=nano
ExecStart=/usr/local/bin/nano_node --daemon --data_path $NANO_DIR
Restart=always
RestartSec=10
LimitNOFILE=65536
[Install]
WantedBy=multi-user.target
EOF
systemctl daemon-reload
systemctl enable --now nano-node
sleep 5
echo "node started; RPC at http://127.0.0.1:7076 (loopback only). Sync progress:"
curl -s -d '{"action":"block_count"}' http://127.0.0.1:7076 || echo "(RPC not up yet)"
echo
echo "Set NANO_RPC_URLS=http://127.0.0.1:7076 in /var/lib/sail/env and restart sailnode once the node is synced."
