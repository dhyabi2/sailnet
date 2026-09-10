# Sailnet

**Website and downloads: [www.sailnet.space](https://www.sailnet.space)**

A rewarded privacy network paid in XNO. Relays carry onion-routed circuits
over ordinary-looking HTTPS and are paid per megabyte, directly, on the Nano
ledger. No token, no accounts, no company in the middle.

- **Run a relay** on any VPS or home PC and earn XNO to a wallet you control.
- **Use the network** from the Android app, the desktop apps (SOCKS5 proxy
  and DNS for any browser), or the Chrome and Firefox extensions.
- **Censorship resistance**: bridges, WebSocket-shaped tunnels, measured
  traffic shaping.

Downloads: the [Releases](../../releases) page has `sailnode` and `sail` for
Linux (x86-64 and arm64), the desktop apps for macOS and Windows, the Android
APK and the Chrome and Firefox extensions, each with a `.sha256` beside it.
Docker images are on GitHub Container Registry as `ghcr.io/dhyabi2/sailnet`.

## Run a relay in one command

> ### Set `--payout` first, before anything else
>
> `--payout nano_…` is the address your earnings are sent to, and it is the
> one decision you should make before you start the relay rather than after.
>
> **Without it the relay still earns — into a wallet that exists only on that
> server.** Nothing is lost while the machine lives, but the seed is in
> `SAIL_HOME/wallet.json` and nowhere else: no copy, no account, no support
> address. Destroy the VPS, lose the disk, or rebuild the box without
> exporting that seed, and the money is gone, permanently, and nobody can
> give it back. That is the same property that makes the network impossible
> to freeze.
>
> With `--payout` set, everything above a small operating float is swept out
> every 15 minutes to a wallet you already control, so the server never holds
> more than a few minutes of earnings and can be thrown away at any moment.
>
> Use any Nano address you own — Natrium, Nault, an exchange deposit address.
> It costs nothing to set and cannot be set too early.
>
> **Already running without it?** Nothing is lost yet. Back the wallet up, or
> move the earnings out and add the flag. Note the `SAIL_WALLET=` prefix: run
> on a relay, `sailnode wallet` finds the wallet the *installed service* uses,
> while plain `sail` would reach for `~/.sail` and send from the wrong (likely
> empty) wallet.
>
> ```
> sailnode wallet export                     # the seed and address holding your earnings — write it down
> W=$(sailnode wallet where 2>/dev/null)     # the path the service really uses
> SAIL_WALLET=$W sail wallet show            # what has accumulated
> SAIL_WALLET=$W sail send nano_your_wallet 0.5
> # then add --payout nano_your_wallet to the unit and: systemctl restart sailnode
> ```

Docker:

```
docker run -d --name sailnet --restart unless-stopped -p 443:443 -v sailnet:/data \
  ghcr.io/dhyabi2/sailnet relay --register --payout nano_your_wallet_address_here
```

Binary (Linux, x86-64 or arm64, as root, port 443; the checksum is verified):

```
arch=$(uname -m | sed 's/x86_64/amd64/; s/aarch64/arm64/')
curl -fsSL -o /usr/local/bin/sailnode https://github.com/dhyabi2/sailnet/releases/latest/download/sailnode-linux-$arch \
  && curl -fsSL https://github.com/dhyabi2/sailnet/releases/latest/download/sailnode-linux-$arch.sha256 | sed 's#  .*#  /usr/local/bin/sailnode#' | sha256sum -c \
  && chmod +x /usr/local/bin/sailnode
sailnode relay --register --payout nano_your_wallet_address_here
```

The command is `sailnode` — Sailnet is the network's name, not a binary. The
other command a release ships is `sail`, the wallet CLI.

To keep it running across reboots:

```
cat > /etc/systemd/system/sailnode.service <<'EOF'
[Unit]
Description=Sailnet relay
After=network-online.target
[Service]
Environment=SAIL_HOME=/var/lib/sailnode
ExecStart=/usr/local/bin/sailnode relay --register --payout nano_your_wallet_address_here
Restart=always
RestartSec=5
KillSignal=SIGTERM
TimeoutStopSec=30
AmbientCapabilities=CAP_NET_BIND_SERVICE
[Install]
WantedBy=multi-user.target
EOF
systemctl enable --now sailnode
```

`SAIL_HOME` is where the node keeps its wallet and its state. Set it in the
unit as above: systemd starts services with no `HOME`, and a node that has to
guess picks a directory you would not think to look in. Docker sets it to
`/data` already.

The node creates its own wallet there on first start, and reuses that one for
ever after — reinstalling, upgrading or restarting never mints a second seed
over the top of an existing one. **Back it up** (`sailnode wallet export`):
those earnings have no other copy, which is the whole point of `--payout`
above. An empty wallet asks the Sailnet faucet for the registration amount by
itself, so nothing has to be sent by hand; if the faucet is unavailable the
log names the amount (0.0005 XNO) and the address. Prepayments to the relays it forwards to come out of its own
earnings. `sailnode relay -h` lists every flag; the useful ones:

| flag | what it does |
|---|---|
| `--payout nano_…` | **set this first.** Forward earnings to a wallet you control, checked every 15 minutes. Without it they accumulate in `SAIL_HOME/wallet.json` on the server, and that seed is the only copy in existence |
| `--ip 203.0.113.7` | the public IPv4 published on the ledger; detected automatically when omitted |
| `--cc DE` | ISO country code published on the ledger, so clients can pick paths across countries and exits by country; optional (`XX`) |
| `--owner nano_…` | a wallet that rides this relay free, set by hand. Default: `--payout`. Pairing from the app (`sailnode pair`) adds more. See *Run a relay, ride it free* |
| `--spot-discount 50` | sell idle capacity cheaper: percent off, signed into gossip in 30-minute windows while load is under `--spot-below` (default 20% of `--capacity-mbps`). Off by default. See *Cheaper when it is quiet* |
| `--rep-friends nano_…,nano_…` `--rep-bonus 20` | more bytes per XNO for payers whose account votes for a representative you respect. Your opinion, as a price. Off by default |
| `--payout-keep 0.5` | XNO kept on the node as float for prepaying the next hop; everything above it is forwarded. Left unset it sizes itself: what eight pool top-ups cost at the peers' published prices. It follows what prepaying actually costs, and raising your own `--rate` does not move it — what you charge should not decide when you get paid |
| `--rate 0.0005` | starting price in XNO per MiB (about $0.20 per GB); `--reprice` adjusts it to demand every 10 days: down 10% when usage falls, up 3% when it grows, never above four times the start. Changing this flag overrides whatever demand had done to the price |
| `--min-rate 0.0002` | the price floor `--reprice` may never go under (default: a quarter of `--rate`). Set it to what serving a MiB actually costs you and the price looks after itself from then on |
| `--pool-mib 32` | MiB of service each prepayment to the next hop buys, at that relay's own published price. Sized in service rather than in XNO, so a relay that charges more is still prepaid rather than quietly skipped |
| `--min-credit-mib 10` | the smallest quota one payment may open, granted once per paying wallet per day. It keeps an app built against an older, cheaper price usable on your relay instead of leaving it to re-anchor in a loop; `0` turns it off |
| `--host relay.example.org --acme` | a real domain and an automatic Let's Encrypt certificate |
| `--listen :443,:8443` | extra ports, printed as bridge lines |
| `--unlisted` | run as a bridge: never on the ledger, handed out by invite |
| `--exit=false` | middle relay only |
| `--rpc http://127.0.0.1:7076` | Nano RPC endpoint(s), comma-separated, tried in order; default Sailnet's endpoint, then public nodes |
| `--rpc-key …` | API key, only if `--rpc` is rpc.nano.to |

## Run a relay, ride it free

If you run a relay, your own app uses it without paying. The relay names an
owner — the wallet its earnings go to (`--payout`), or one you give with
`--owner nano_…` — and a circuit that wallet opens on that relay is admitted
on a signature alone: no payment, no ledger lookup. The hops beyond it are
prepaid from the relay's float the way every circuit is, so your traffic is
paid for out of what your relay earned, which was your money already. Nobody
else's relay carries anything unpaid; every other operator earns exactly what
they would have.

**Pair it — six digits, nothing else to copy.** On the relay:

```
sailnode pair
Pairing code:  483 920      (valid five minutes, one use)
```

In the app: Settings → *Run a relay, ride it free* → **Add my relay** → pick
the relay, enter the code. *Paired relays* lists what this phone has paired
and can forget them; nothing is typed by hand. The app pays that relay an ordinary anchor, opens
a circuit to it and sends the code inside; the relay records the wallet that
paid as an owner. A relay that has no owner yet prints a code by itself when
it starts (`docker logs sailnet` shows it). On the command line the same is
`sailnode pair-relay nano_<relay> 483920`, then `--mine nano_<relay>`.

Then choose how your relays are used — Settings → **Network**:

| mode | path | you pay | who sees your address |
|---|---|---|---|
| **Direct** | your relay only, 1 hop | nothing | your relay (and its host); websites see the relay. A VPN through a box you own |
| **My relays** | stranger → stranger → your relay | one entry anchor | the entry only; nobody watching your relay sees you arrive |
| **Open network** | three strangers | full price | the entry only; your relays are not used |

Direct is for the owner of one relay who does not want to pay two strangers;
My relays is for when you would rather not be seen arriving at your own. All
three use the same link shaping: measured on a live relay, the cover cadence
that hides the link's rhythm is also the faster link (a profile without it
moved 20 MB at a third of the speed), so there is nothing to switch off.
Changing *Network* or pairing a relay while connected reconnects the tunnel
by itself, so the setting takes effect at once. If
your relay is down in Direct, the app says so and offers the other two rather
than silently paying. `--owner nano_…` (default `--payout`) still works as
before for a wallet you set on the relay yourself. The owner tag is bound to
the relay it is for and signed with your key, so it means nothing anywhere
else, and to everyone but your relay the circuit looks like any other.

Nano fans have run representatives for years for nothing. This is the same
arrangement with something in it for you: the network you help carry is the
one you browse through.

## Watching your relay

The node keeps running in the terminal you started it in and does not read
what you type there. Open a **second terminal** and run:

```
sailnode stats             # this relay now: MiB relayed, circuits open, uptime, and the network it sees
sailnode stats --watch 2   # the same every two seconds, with the rate
```

If you run the downloaded file directly rather than installing it as
`sailnode`, the command is `./sailnode-linux-amd64 stats` (or whatever the
file is called).

It asks the relay's own `/stats` on loopback — one HTTPS request, read from
memory. No ledger, no RPC, nothing on the network. `--addr` if the relay does
not listen on `127.0.0.1:443`. Not yet for `earn --home` nodes: a home node
has no local port, so there is nothing for `stats` to ask; it is earning all
the same.

## Cheaper when it is quiet, and for good Nano citizens

Two things a relay can switch on alone. Neither needs anyone else to agree,
and neither makes any relay carry a byte for less than it asked.

**Spot price.** `--spot-discount 50` makes an idle relay sign a lower price
into its own gossip record for the next thirty minutes; clients that see the
offer pay that price and the relay credits at it for the whole window, busy
or not. It is the fast version of the repricing relays already do every ten
days, and it is the relay's own signature over its own price, so a client can
hold it to it. `sailnode relays` shows standing offers.

**Representative-aligned price.** `--rep-friends nano_…,nano_… --rep-bonus 20`
credits a fifth more bytes per XNO to a payer whose account votes for one of
the representatives you list. The vote is in the payment block the relay
already reads, so it costs nothing to check and reveals nothing the payment
did not. It changes bytes per XNO only — never who gets chosen.

**The meter.** `sailnode costs` (and the app's main screen) shows what this
wallet paid, to which relay, at what price, and how much of it was used — from
a ledger that lives on the device and nowhere else.

## Upgrading

```
sailnode upgrade            # fetch the newest published build, verify it, install it, restart
sailnode upgrade -check     # say whether you are up to date, change nothing
sailnode upgrade -restart=false   # install now, restart when you choose
```

The download is checked against the SHA-256 published beside it and nothing is
replaced until it matches, so a failed or tampered download leaves the running
node untouched. The previous binary is kept next to the new one as
`sailnode.previous`. Your wallet, your quota log and everything else in
`SAIL_HOME` are never read or written by an upgrade.

The new build is then run once, before the service is restarted. A binary that
cannot execute — the wrong architecture, a download truncated along with its
checksum file, a missing library — would otherwise be found only by a service
that restarts forever, which is the one failure an operator does not watch
happen. If it does not run, the previous build goes back and nothing is
restarted. `sail`, the wallet CLI, is upgraded at the same time when it is
already installed beside `sailnode`; an upgrade never adds a command you did
not choose to have.

Set `SAIL_RELEASE_API` to upgrade from somewhere else — a fork, a mirror, an
air-gapped copy. The download is still verified against the `.sha256` published
beside it, so pointing this elsewhere buys a different source, not a weaker one.

Upgrading is optional. A relay that never upgrades keeps working, keeps being
chosen for circuits, and keeps earning: the network is built so that no
release can require an operator to act.

**Nano RPC.** Clients and relays read the ledger through
`https://www.sailnet.space/node/api` by default, Sailnet's own endpoint,
which forwards to rpc.nano.to with Sailnet's key and fails over to public
nodes. The apps' settings let you use rpc.nano.to directly with your own key,
or any node you run; the CLI takes `--rpc` and `--rpc-key` (or `NANO_RPC_URLS` and
`NANO_RPC_KEY`).

A relay works out of the box through Sailnet's endpoint; no Nano node to
install. If you run one anyway, pass `--rpc http://127.0.0.1:7076`.

Home PC behind NAT, no port forwarding: `sailnode earn --home --payout nano_…`.

## Upgrading the app

**Relays (Linux).** `sailnode upgrade` is the whole procedure. It verifies the
published SHA-256 before replacing anything, keeps the previous binary beside
the new one, and never touches your wallet or any other file in `SAIL_HOME`.
Upgrading is optional: a relay that never upgrades keeps working, keeps being
chosen, and keeps earning.

**Desktop (Windows, macOS).** Download and install over the top. The wallet
lives in `~/.sail`, outside the app, so it survives.

**Android — one-time reinstall.** Every release before v0.2.53 was signed with
a throwaway key that changed on each build, so Android refuses to install a
newer version over an older one. Moving to v0.2.53 needs an uninstall and a
fresh install, once. **Back up your seed first** (Settings → Back up wallet)
if the wallet holds anything you want to keep — uninstalling deletes it. From
v0.2.53 onward every release is signed with the same key and installs over the
last one normally.

## Your wallet, and backing it up

A relay's earnings and a client's balance live in one secret: a 32-byte seed
in `wallet.json`. Nobody else has a copy. There is no account to recover and
no support address that can give it back, because a network where somebody
could return your money is a network where somebody could take it.

So write it down.

```
sailnode wallet export      # show the address and the seed to keep
sailnode wallet import SEED # put a saved wallet back (also accepts a file, or -)
sailnode wallet where       # the path it is stored at
```

Run from a shell on a relay, these find the wallet the installed service
actually uses, not the one your login account would have: the unit is read
for its data directory. Nothing is guessed, and an explicit `SAIL_HOME` or
`SAIL_WALLET` always wins.

The apps have the same two actions. On Windows, macOS and Linux they are in
Settings under **Wallet**: *Back up wallet* shows the seed and can save it to
a file, *Restore wallet* takes it back. On Android they are in Settings under
**Wallet** as well, and they matter most there: uninstalling an Android app
deletes everything it stored, this wallet included.

Two guarantees hold everywhere:

- **An existing wallet is always reused.** Installing again, upgrading, or
  starting a relay for the tenth time never mints a new seed over an old one.
  A wallet file that is present but unreadable is left exactly as it is and
  reported, never replaced.
- **A restore keeps what it replaces.** The wallet that was there is renamed
  beside the new one as `wallet.json.replaced-<time>`, so a seed typed from
  the wrong piece of paper is not the end of the story.

Restore with the tunnel disconnected. Circuits already open are prepaid out
of the wallet you are replacing.

## Use the network

```
sailnode client --socks 127.0.0.1:1080 --hops 3
curl --socks5-hostname 127.0.0.1:1080 https://api.ipify.org
```

The client makes a wallet on first run and claims a free trial grant for it,
so the first circuit costs nothing and there is no step where you have to buy
XNO before seeing whether the thing works. After that it pays its own way: it
prepays each entry relay for about 10 MiB at that relay's published price, and
tops up from the circuit's measured rate. The bridge and censorship defences
are always on and have no switch; a bridge line from someone who runs a bridge
is the only thing worth adding by hand. `sailnode client -h` lists whole-device
capture, DNS through the circuit and the status endpoint for the browser
extension. Android: install the APK from Releases; the app funds itself the
same way and shows where to get XNO.

## Built to be hard to censor

Every one of these is in the code today, not a plan.

- **One connection, ever.** A client talks to nothing but its entry relay.
  The first ledger read of a fresh wallet, and the watch for its first
  payment, go through the entry on the tunnel's control channel: the entry
  forwards a small, rate-limited set of ledger requests and pushes the
  confirmation the moment it lands. No Nano node, no website, no WebSocket
  is ever contacted by the client, and every block is signed on the client.
- **Cadence on the entry link.** Both ends of the client-to-entry connection
  send at least one cell every 25 ms, padding when idle, so the link's rhythm
  no longer follows what the user does; relays batch with randomised timing
  so single flows lose their shape in the aggregate.
- **Plain HTTP refused.** Port 80 never leaves the exit unless the operator
  explicitly allows it; only encrypted destinations do.
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
- **Censored-network profile, always on.** There is no switch and no flag to
  forget: bridges are preferred as entries, listed relays are never probed
  from the real address, gossip is fetched only from bridges, and there is
  never a direct ledger call. A defence you can turn off is one that is off
  for the people who needed it most.
- **No single list to block.** Relays register on the Nano ledger itself,
  which cannot be taken down, and also gossip signed records to each other, so
  a client with one working bridge learns the whole network without the
  ledger. A forged record needs the relay's private key.
- **DNS never leaves the device.** The client answers DNS locally and
  forwards each query through the circuit to a resolver at the exit; the
  Android app and whole-device capture sinkhole every name.
- **No blocking, deliberately.** There is no kill switch and no black hole.
  If the Android client fails to start, the tunnel is torn down and the
  notification says why, so the phone keeps its ordinary connection and the
  user can tap Connect again. A privacy tool that silently takes the network
  away is a tool people uninstall, and while traffic is flowing it goes
  through the circuit or not at all.
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
credit and there is nothing to ban. Earnings are swept to `--payout` every 15
minutes.

Every prepaid amount is a quantity of *service*, never a fixed number of XNO:
an anchor buys about 10 MiB at the entry's published price, a pool buys
`--pool-mib` at the next hop's, and the operating float is what eight top-ups
cost at the peers' published prices — never at your own, so repricing your
relay does not move when you get paid. Prices differ between relays and move over time, so an amount
written in XNO buys the wrong thing the moment either happens — a relay
charging more than a fixed pool was sized for used to be skipped silently
rather than paid, which is a partitioned network and no error message
anywhere.

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
through the circuit, and a failed start tears the tunnel down with the reason
rather than black-holing the phone's network. Chrome: a proxy toggle with a
WebRTC guard.

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

`sail/cmd/sailgui` is the macOS, Windows and Linux client: one small window
with connect, wallet (address, balance, where to get XNO), status and
settings (exit exclusion, bridges). It runs the same client as
`sailnode client` and serves a SOCKS5 proxy on 127.0.0.1:1080 and DNS on
127.0.0.1:5300 for browsers and the extension. Releases carry
`Sailnet-macOS-AppleSilicon.dmg`, `Sailnet-macOS-Intel.dmg` and
`Sailnet-Windows.exe`. Every file in a release has a `.sha256` beside it.

The `Desktop apps` workflow signs and notarizes when the certificates are in
the repository secrets (Developer ID certificate and notarytool credentials
for macOS, a code-signing PFX for Windows; see the workflow header). Without
them the builds are unsigned: macOS needs right-click → Open the first time,
Windows shows SmartScreen's "Run anyway".

Build locally: `cd sail && go build ./cmd/sailgui` (needs a C compiler; on
Windows, MSYS2/mingw-w64).

## Website and brand

`website/` is the static site and its two small API functions (`/api/stats`,
`/api/faucet`), deployed to [www.sailnet.space](https://www.sailnet.space) on
Vercel; the faucet forwards to relays named in its environment.
`brand/` holds the mark and wordmark (SVG) and the app icon, with the rules:
black and white only, no gradients, no rounded corners.

## Build from source

```
cd sail && go build -o bin/sailnode ./cmd/sailnode && go build -o bin/sail ./cmd/sail
go test ./...
```

Android APK from source (needs Go, JDK 17, the Android SDK with NDK 26 and
platform 34):

```
go install golang.org/x/mobile/cmd/gomobile@latest golang.org/x/mobile/cmd/gobind@latest
export ANDROID_HOME=$HOME/Android/Sdk ANDROID_NDK_HOME=$ANDROID_HOME/ndk/26.3.11579264
cd sail && gomobile init && mkdir -p ../android/app/libs
gomobile bind -ldflags="-s -w" -target android/arm64,android/arm,android/amd64 -androidapi 24 \
  -javapkg net.sailnet -o ../android/app/libs/sail.aar ./mobile
cd ../android && ./gradlew assembleDebug
ls app/build/outputs/apk/debug/        # app-universal-debug.apk and per-ABI APKs
```

Desktop app: `cd sail && go install fyne.io/tools/cmd/fyne@latest && cd cmd/sailgui && fyne package -os darwin|windows|linux -icon Icon.png`.
The same steps run in `.github/workflows/android-release.yml` and `desktop.yml`. Every push of a `v*` tag builds the node binaries (Linux x86-64 and arm64), the desktop apps, the browser extensions, the Docker
image and the APK, and attaches them to a release with a `.sha256` each.

## Layout

- `sail/` Go implementation: `nano/` keys and blocks, `wire/` cells and onion
  layers, `relay/` relay server and circuit client, `client/` desktop client,
  `shape/` traffic shaping and its measurement rig, `cmd/` binaries
- `android/`, `extension/` the apps
- `docker/` image and compose files

