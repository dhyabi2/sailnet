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
  ghcr.io/dhyabi2/sailnet relay --register --payout nano_your_wallet_address_here
```

Binary (Linux, as root, port 443):

```
curl -L -o /usr/local/bin/sailnode https://github.com/dhyabi2/sailnet/releases/latest/download/sailnode-linux-amd64 && chmod +x /usr/local/bin/sailnode
sailnode relay --register --payout nano_your_wallet_address_here
```

The node creates its own wallet in `SAIL_HOME` (`/data` in Docker,
`~/.sail` otherwise) on first start and prints its address; send it a little
XNO (0.01 is plenty) so it can publish its registration and prepay the
relays it forwards to. `sailnode relay -h` lists every flag; the useful ones:

| flag | what it does |
|---|---|
| `--payout nano_…` | forward earnings to this wallet every hour |
| `--ip 203.0.113.7` | the public IPv4 published on the ledger; detected automatically when omitted |
| `--cc DE` | ISO country code published on the ledger, so clients can pick paths across countries and exits by country; optional (`XX`) |
| `--payout-keep 0.02` | XNO kept on the node as float (default 0.02) |
| `--rate 0.00002` | price in XNO per MiB |
| `--host relay.example.org --acme` | a real domain and an automatic Let's Encrypt certificate |
| `--listen :443,:8443` | extra ports, printed as bridge lines |
| `--unlisted` | run as a bridge: never on the ledger, handed out by invite |
| `--exit=false` | middle relay only |
| `--rpc http://127.0.0.1:7076` | Nano RPC endpoint(s), comma-separated, tried in order; default Sailnet's endpoint, then public nodes |
| `--rpc-key …` | API key, only if `--rpc` is rpc.nano.to |

**Nano RPC.** Clients and relays read the ledger through
`https://www.sailnet.space/node/api` by default, Sailnet's own endpoint,
which forwards to rpc.nano.to with Sailnet's key and fails over to public
nodes. The apps' settings let you use rpc.nano.to directly with your own key,
or any node you run; the CLI takes `--rpc` and `--rpc-key` (or `NANO_RPC_URLS` and
`NANO_RPC_KEY`).

A relay works out of the box through Sailnet's endpoint. For the most
private setup run your own Nano node (`deploy/nano-node.sh`) and pass
`--rpc http://127.0.0.1:7076`.

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

## Built to be hard to censor

Every one of these is in the code today, not a plan.

- **Looks like a website.** A relay is an HTTPS server with a real decoy site.
  The client sends a current Chrome ClientHello (uTLS) with the relay's
  hostname as SNI, then a standard WebSocket upgrade; only a path carrying a
  daily token derived from the relay key opens the tunnel. Any other request,
  including active probes that replay or guess paths, gets the decoy page and
  an identical 404.
- **Real certificates.** `--acme` fetches Let's Encrypt certificates for a
  real domain, so nothing is self-signed; the circuit handshake signs the
  SHA-256 of the live leaf with the relay's ledger key, so a middlebox that
  forges a certificate is caught inside the tunnel, not trusted.
- **Real WebSocket framing.** Cells travel in RFC 6455 frames, so an inspector
  that follows the handshake sees a well-formed WebSocket and a bridge can sit
  behind a WebSocket-aware CDN or reverse proxy.
- **Traffic shaping that was measured, not assumed.** The tunnel writer
  batches cells with quiet-gap coalescing, writes full-size TLS records, holds
  back sub-record remainders and replays the first records of a connection
  from a profile of real HTTPS. `sailtrace` captures record-level traces under
  TLS the way a DPI box sees them, trains a random forest over the published
  TLS-in-TLS feature set plus the encapsulated-handshake rule, and the shipped
  parameters are the ones tuned against it on the live network.
- **No fixed rhythm.** Keepalives are jittered; there is no periodic pattern
  to key on.
- **Bridges with secrets.** `relay --unlisted` never touches the ledger and
  hands out a bridge line with a 16-byte secret; the tunnel token is derived
  from the key and the secret, so someone who read the ledger and has the
  address still gets the decoy. Relays can listen on several ports; the extra
  ports are bridge lines, so blocking one port is not enough.
- **Metered bridge distribution.** `bridgedb` hands out a few bridges per
  invite code from buckets; reports from three distinct invites retire a
  bridge and refill the buckets, so an enumerator burns invites, not the
  network.
- **Bootstrap without any Nano node.** A client that holds a bridge secret
  gets a small free circuit to reach the ledger through the bridge and fund
  itself. In stealth mode the client never contacts a Nano node at all: it
  signs its payment offline from a cached chain state and the entry relay
  publishes it, and every later ledger call goes through the circuit's exit.
- **Censored-network profile.** `--censored` (the app's switch): bridges are
  the only entries, listed relays are never probed from the real address,
  gossip is fetched only from bridges, and there is never a direct ledger call.
- **No single list to block.** Relays register on the Nano ledger itself,
  which cannot be taken down, and also gossip signed records to each other, so
  a client with one working bridge learns the whole network without the
  ledger. A forged record needs the relay's private key.
- **DNS never leaves the device.** The client answers DNS locally and
  forwards each query through the circuit to a resolver at the exit; the
  Android app and whole-device capture sinkhole every name.
- **Kill switch.** On Android the tunnel stays up as a black hole if the
  client fails to start; nothing falls back to the open network.
- **Home relays.** A PC behind NAT registers with its harbour's country and no
  ASN, and reaches the harbour through an ingress circuit, so the ledger never
  says where the operator is and there is a supply of residential entries.
- **Payment that cannot be frozen.** No accounts, no token, no company: an XNO
  send is the ticket, and anyone can run a relay and be paid.

Nothing here has been measured from inside a censored network yet; the
measurement rig and the bridge distribution service are what that will be
done with.

### macOS: "cannot be opened because the developer cannot be verified"

That message is Gatekeeper reporting that the build is unsigned, which is the
case until the repository has an Apple Developer ID certificate. Open it once
with right-click → Open (or System Settings → Privacy & Security → Open
Anyway); after that it launches normally. To make the warning disappear for
everyone, join the Apple Developer Program (US$99/year, any legal entity or
person; a domain such as sailnet.space is not enough on its own), create a
*Developer ID Application* certificate and an app-specific password, and add
`MACOS_CERT_P12_BASE64`, `MACOS_CERT_PASSWORD`, `MACOS_SIGN_IDENTITY`,
`APPLE_ID`, `APPLE_TEAM_ID` and `APPLE_APP_PASSWORD` as repository secrets.
The next tag is then signed and notarized automatically. Windows is the same
story with a code-signing certificate in `WINDOWS_CERT_PFX_BASE64`.

## How it works

**Circuits.** A client builds a telescoping multi-hop circuit (two to four relays, three by default): CREATE to the
entry, then EXTEND through each established hop, with an X25519 handshake per
hop. Every cell is 1 024 bytes and carries one ChaCha20-Poly1305 layer per hop,
peeled at each relay; nonces are per-hop, per-direction sequence numbers with a
64-cell anti-replay window, so no relay can replay or reorder cells to tag
traffic. The entry sees the client, the exit sees the destination, nobody sees
both. Streams inside a circuit carry TCP, UDP (datagram streams, length-framed)
and DNS to the exit.

**Payment.** No token and no accounts: the client sends XNO to the entry relay
and the send's block hash is the circuit tag. In stealth mode the client never
contacts a Nano node; it signs the block offline from a cached chain state and
hands it to the entry, which publishes and verifies it. Relays prepay the next
hop from pooled sends and meter every cell against quota, so nobody extends
credit and there is nothing to ban. Earnings are swept hourly to `--payout`.

**Registry.** Relays register on the Nano ledger itself: REGISTER and
DESCRIPTOR operations encoded in the representative field of state blocks
sent to the treasury account, so the relay list is public, verifiable and
needs no server. Relays also sign their own records and gossip them, so a
client with a single bridge line and no ledger in reach still learns the
network; forged records need the relay's private key.

**Transport.** A relay is an HTTPS site. The client sends a current Chrome
ClientHello (uTLS) with the relay's hostname as SNI, then a standard WebSocket
upgrade whose path carries a daily token derived from the relay key; any other
path gets the decoy website with an identical 404 for probes. Cells ride in
real RFC 6455 frames, so a bridge can sit behind a WebSocket-aware CDN.
`--acme` fetches Let's Encrypt certificates; the CREATED ack signs the SHA-256
of the live TLS leaf with the relay's ledger key, so a forged certificate is
caught inside the circuit handshake.

**Traffic shaping, measured.** The tunnel writer batches cells asynchronously
with quiet-gap coalescing (30 ms, capped at 250 ms or 16 KiB), writes
full-size TLS records, holds back sub-record remainders, and replays the first
records of a connection from a profile of real HTTPS. `sailtrace` taps the
TCP stream under TLS, reconstructs records the way a DPI box does, trains a
random forest over the published TLS-in-TLS feature set plus the
encapsulated-handshake rule, and tunes the parameters against it on the live
network. The shipped defaults are the measured ones.

**Bridges.** `relay --unlisted` never touches the ledger and prints a bridge
line with a secret; the token is derived from key and secret, so a prober who
read the ledger still gets the decoy. Holders of a secret get a small free
bootstrap circuit to fund themselves. `bridgedb` hands out a few bridges per
invite code and retires burned ones from reports. A relay can serve on
several ports; the extra ports are bridge lines.

**Home nodes.** `sailnode earn --home` runs a relay on a PC behind NAT:
NAT-PMP/PCP and UPnP with retransmits, a public-IP truth check against
carrier-grade NAT, and when the PC cannot be reached, an outbound tunnel to a
public relay (its harbour) that bridges circuits onto it, with the harbour's
country in the registry so the ledger never says where the operator is.

**Rewards.** Every relay owes 10 % of each day's earnings to the others, 60 %
by age and 40 % by performance, paid peer to peer with epoch-tagged sends;
clients recompute the table from the ledger and draw paths by a weighted
lottery that excludes non-payers. Opt-in with `--levy`.

**Clients.** Desktop: SOCKS5 with remote DNS, a DNS resolver that forwards
through the circuit, optional whole-device capture (DNS sinkhole plus Host/SNI
listeners), a status endpoint that only browser extensions may read, and a
nickname that replaces the wallet address and device addresses in every log.
Android: a VpnService with a userspace network stack routes all TCP and UDP
through the circuit, and a kill switch keeps the tunnel as a black hole if the
client fails to start. Chrome: a proxy toggle with a WebRTC guard.

**Privacy limits, stated plainly.** Sailnet cannot hide the device's MAC
address or hostname on the local network, or the funding graph of a wallet on
the public ledger; fund relay and client wallets from an exchange if they must
not be linkable to you.

## Browser extensions

`extension/` is one code base for Chrome and Firefox: a proxy toggle that
points the browser at the local client's SOCKS5 port with remote DNS, a
WebRTC leak guard, and the live circuit status from the client's status
endpoint. Releases carry `sailnet-chrome.zip` and `sailnet-firefox.zip`.

- Chrome: `chrome://extensions`, Developer mode, "Load unpacked" on the
  unzipped folder (or install from the Chrome Web Store once listed).
- Firefox: `about:debugging`, "Load Temporary Add-on" on the zip (or install
  from addons.mozilla.org once listed).

`extension/build.sh` packages both. The `Browser extensions` workflow lints
the Firefox build and, when the store credentials are set as repository
secrets (see the workflow header), uploads to the Chrome Web Store and
submits to addons.mozilla.org on every version tag.

## Desktop apps

`sail/cmd/sailgui` is the macOS and Windows client: one small window with
connect, wallet (address, balance, where to get XNO), status and settings
(hops, exit country, censored mode, bridges). It runs the same client as
`sailnode client` and serves a SOCKS5 proxy on 127.0.0.1:1080 and DNS on
127.0.0.1:5300 for browsers and the extension. Releases carry
`Sailnet-macos-arm64.dmg`, `Sailnet-macos-amd64.dmg`,
`Sailnet-windows-amd64.exe` and `Sailnet-linux-amd64.tar.xz`. Every file on
a release has a `.sha256` next to it; the node binaries also share one
`SHA256SUMS`.

The `Desktop apps` workflow signs and notarizes when the certificates are in
the repository secrets (Developer ID certificate and notarytool credentials
for macOS, a code-signing PFX for Windows; see the workflow header). Without
them the builds are unsigned: macOS needs right-click → Open the first time,
Windows shows SmartScreen's "Run anyway".

Build locally: `cd sail && go build ./cmd/sailgui` (needs a C compiler; on
Windows, MSYS2/mingw-w64).

## Website and brand

`website/` is the static site, published by GitHub Pages from `main`.
`brand/` holds the mark and wordmark (SVG) and the app icon, with the rules:
black and white only, no gradients, no rounded corners.

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
