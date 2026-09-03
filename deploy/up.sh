#!/bin/bash
# Create three micro droplets in three regions and install Sailnet relays.
# Requires: doctl authenticated (doctl auth init), ssh key ~/.ssh/id_ed25519.
# Each relay gets its own Nano wallet (seed generated here, stored in deploy/relays.json);
# fund each with a little XNO (~0.01) so it can publish REGISTER/DESCRIPTOR blocks.
set -euo pipefail
cd "$(dirname "$0")/.."
REGIONS=(${REGIONS:-ams3 sgp1 nyc3})
CCS=(NL SG US)
ASNS=(14061 14061 14061)   # DigitalOcean; self-declared, verified off-chain by clients
RATE=${RATE:-0.5}
SIZE=${SIZE:-s-1vcpu-512mb-10gb}
IMAGE=ubuntu-24-04-x64
KEYID=$(doctl compute ssh-key list --format ID --no-header | head -1)
[ -n "$KEYID" ] || { doctl compute ssh-key import sail --public-key-file ~/.ssh/id_ed25519.pub; KEYID=$(doctl compute ssh-key list --format ID --no-header | head -1); }
mkdir -p deploy/state
for i in 0 1 2; do
  r=${REGIONS[$i]}; name=sail-$r
  seed=$(openssl rand -hex 32)
  echo "$seed" > deploy/state/$name.seed; chmod 600 deploy/state/$name.seed
  doctl compute droplet create "$name" --region "$r" --size "$SIZE" --image "$IMAGE" --ssh-keys "$KEYID" --tag-name sailnet --wait --format ID,Name,PublicIPv4 --no-header | tee -a deploy/state/droplets.txt
done
sleep 20
for i in 0 1 2; do
  r=${REGIONS[$i]}; name=sail-$r
  ip=$(doctl compute droplet get "$name" --format PublicIPv4 --no-header)
  seed=$(cat deploy/state/$name.seed)
  sed -e "s/__CC__/${CCS[$i]}/; s/__ASN__/${ASNS[$i]}/; s/__SEED__/$seed/; s/__IP__/$ip/; s/__RATE__/$RATE/" deploy/cloud-init.sh > deploy/state/$name.init.sh
  until ssh -o StrictHostKeyChecking=no -o ConnectTimeout=5 root@$ip true 2>/dev/null; do sleep 5; done
  rsync -az --exclude bin --exclude .git -e "ssh -o StrictHostKeyChecking=no" sail/ root@$ip:/opt/sail/
  scp -o StrictHostKeyChecking=no deploy/state/$name.init.sh root@$ip:/root/init.sh
  addr=$(cd sail && SAIL_WALLET=/dev/stdin go run ./cmd/sail wallet 2>/dev/null <<<"{\"seed\":\"$seed\",\"index\":0}" | head -1 || true)
  echo "$name $ip $addr" | tee -a deploy/state/relays.txt
done
echo
echo "Fund each relay address above with ~0.01 XNO, then run: deploy/finish.sh"
