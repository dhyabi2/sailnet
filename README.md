# Sailnet

A rewarded privacy network paid in XNO. Relays carry onion-routed circuits
over ordinary-looking HTTPS and are paid per megabyte, directly, on the Nano
ledger. No token, no accounts, no company in the middle.

- **Run a relay** on any VPS or home PC and earn XNO to a wallet you control.
- **Use the network** from the Android app, the desktop client (SOCKS5 proxy
  and DNS), or the Chrome extension.
- **Censorship resistance**: bridges, WebSocket-shaped tunnels, measured
  traffic shaping.

Downloads: the [Releases](../../releases) page has `sailnode` for Linux,
macOS and Windows and the Android APK. Docker images are on GitHub Container
Registry as `ghcr.io/dhyabi2/sailnet`.

## Run a relay in one command

Earnings are swept every hour to the wallet you name in `--payout`; the node
keeps only a small operating float. Use any Nano wallet address you own
(Natrium, Nault, an exchange deposit address).

Docker:

```
docker run -d --name sailnet --restart unless-stopped -p 443:443 -v sailnet:/data \
  ghcr.io/dhyabi2/sailnet relay --listen :443 --ip <your public IP> --cc <country code> \
  --register --payout nano_your_wallet_address_here
```

Binary (Linux, as root, port 443):

```
curl -L -o /usr/local/bin/sailnode https://github.com/dhyabi2/sailnet/releases/latest/download/sailnode-linux-amd64 && chmod +x /usr/local/bin/sailnode
sailnode relay --listen :443 --ip <your public IP> --cc <country code> --register --payout nano_your_wallet_address_here
```

The node creates its own wallet in `SAIL_HOME` (`/data` in Docker,
`~/.sail` otherwise) on first start and prints its address; send it a little
XNO (0.01 is plenty) so it can publish its registration and prepay the
relays it forwards to. `sailnode relay -h` lists every flag; the useful ones:

| flag | what it does |
|---|---|
| `--payout nano_…` | forward earnings to this wallet every hour |
| `--payout-keep 0.02` | XNO kept on the node as float (default 0.02) |
| `--rate 0.00002` | price in XNO per MiB |
| `--host relay.example.org --acme` | a real domain and an automatic Let's Encrypt certificate |
| `--listen :443,:8443` | extra ports, printed as bridge lines |
| `--unlisted` | run as a bridge: never on the ledger, handed out by invite |
| `--exit=false` | middle relay only |

A relay should read the ledger from its own Nano node
(`deploy/nano-node.sh` installs one; set `NANO_RPC_URLS=http://127.0.0.1:7076`).
Without one, add `--allow-public-rpc`; payments are then visible to a public
RPC provider.

Home PC behind NAT, no port forwarding: `sailnode earn --home --payout nano_…`.

## Use the network

```
sailnode client --socks 127.0.0.1:1080 --hops 3
curl --socks5-hostname 127.0.0.1:1080 https://api.ipify.org
```

The client makes a wallet on first run; fund it with a few thousandths of an
XNO. On a censored network use `--censored` and a bridge line
from someone who runs a bridge. `sailnode client -h` lists whole-device
capture, DNS through the circuit and the status endpoint for the browser
extension. Android: install the APK from Releases; the app funds
itself the same way and shows where to get XNO.

## Build from source

```
cd sail && go build -o bin/sailnode ./cmd/sailnode && go build -o bin/sail ./cmd/sail
go test ./...
```

Android: `.github/workflows/android-release.yml` shows the gomobile and
Gradle steps. Every push of a `v*` tag builds the node binaries, the Docker
image and the APK and attaches them to a release.

## Layout

- `sail/` Go implementation: `nano/` keys and blocks, `wire/` cells and onion
  layers, `relay/` relay server and circuit client, `client/` desktop client,
  `shape/` traffic shaping and its measurement rig, `cmd/` binaries
- `android/`, `extension/` the apps
- `docker/`, `deploy/` images and server bootstrap

Treasury: `nano_1cexacexqrr51coik9eeqzebmfej7g6chrd1eygmzm9ad86qek84pnhpda1t`
