#!/bin/sh
# Each container derives its Nano key from SEED (hex) so identities are stable.
set -e
mkdir -p "$SAIL_HOME"
if [ ! -f "$SAIL_HOME/wallet.json" ]; then
  if [ -z "$SEED" ]; then SEED=$(head -c 32 /dev/urandom | od -An -tx1 | tr -d ' \n'); fi
  echo "{\"seed\":\"$SEED\",\"index\":0}" > "$SAIL_HOME/wallet.json"
fi
exec sailnode "$@"
