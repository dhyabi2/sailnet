# Rule 1: the network is live. Never break a running node.

The other standing rules (no centralization, no central custody,
anti-manipulation) are in `RULES.md`.

Relay operators are strangers. They will not upgrade when we release, and we
cannot ask them to. From now on every change is judged against one question:

> **Does a node that never upgrades keep working, keep being chosen, and keep
> earning exactly as before?**

If the answer is no, the change does not ship, whatever it improves.

## What this forbids

1. **No wire-protocol changes that a node must adopt.** Cell size, header
   layout, handshake order, onion layers, existing command numbers and their
   payloads are frozen. A new command number is allowed only when an old node
   that receives it ignores it safely (verify the `default:` path, do not
   assume it).
2. **No new requirement to be listed, chosen or paid.** If new code needs a
   signal that only new nodes emit (a heartbeat block, a flag, a field), an
   old node that never emits it must still be listed, still be chosen for
   paths, and still be paid. The signal may only add confidence, never gate
   participation.
3. **No ledger rule that retires an old record.** Registrations are permanent.
   Liveness is decided by measurement (a probe that connects, a gossip record
   that arrives), never by the age of a ledger block, because an old node has
   no way to refresh it.
4. **No change to what a peer must send us.** We may become more careful about
   what we send, never stricter about what we accept.
5. **No default that changes an existing node's behaviour.** Defaults apply to
   new installs. An operator's running command line and stored settings keep
   their meaning.

## What this allows

- Anything inside our own nodes and our own apps: what we choose, what we
  prepay, what we display, how we retry.
- Additive protocol features negotiated per connection or per circuit, where
  the absence of the feature is a working path (windowed streams and cadence
  mode are the pattern to copy: a flag advertises support, and a peer without
  it gets the old behaviour).
- Being *more* generous to old nodes: paying them, routing through them,
  counting them.

## Checklist before every release

- [ ] Old relay, new client: still chosen, still paid, no error it cannot answer.
- [ ] New relay, old client: still builds circuits, no unknown command reaches
      a path that closes the connection.
- [ ] Old client, new relay: same.
- [ ] No code path drops a peer for lacking something only a new build sends.
- [ ] Money: no change makes an existing operator earn less for the same work.

## Log of compatibility decisions

- **2026-09-05, AliveTTL reverted.** v0.2.37 retired ledger registrations
  without a REGISTER, DESCRIPTOR or ALIVE block for three days. Old relays do
  not publish ALIVE blocks, so a stable old relay would have dropped out of
  the registry and stopped earning. The registry keeps every record again;
  liveness is decided by probes and gossip freshness, which old nodes produce
  too. `OpAlive` stays as an extra, optional signal.

- **2026-09-07, the price rose tenfold and every prepaid amount stopped being
  a constant.** The default relay price went from 0.00005 to 0.0005 XNO/MiB
  so that a relay covers a cheap VPS from about 150 GB a month instead of
  1,500. On its own that would have broken three things, all for the same
  reason: the amounts the network prepays were written in XNO, and a price
  they were not chosen for makes them buy the wrong amount of service.

  - A relay refused to prepay a peer whose price made its pool buy under
    8 MiB (`ensurePool`), so a dearer relay was silently never paid and
    never used — a price rise partitioned the network. Pools are now sized
    in bytes at the peer's own price (`--pool-mib`, default 32), with
    `--pool` as the floor, so an existing `--pool 0.001` command line keeps
    its meaning and starts working at any price.
  - A client's anchor bought a tenth of what it used to, so a circuit ran
    out almost at once and the app spent its session re-anchoring. Anchors
    are now sized the same way (`AnchorBytes`, 10 MiB at the entry's price),
    with `--anchor` as the floor.
  - `--payout-keep` left a float too small to prepay anything. Empty now
    means eight pools' worth at the node's own price.

  What this could not fix is an app already installed: its anchor is a fixed
  XNO amount compiled into a build we cannot reach. Relays therefore grant
  `--min-credit-mib` (10 MiB) to any payment that buys less, once per paying
  wallet per day — being more generous to old clients, which this document
  allows, rather than leaving them to fail. An old relay that never upgrades
  still earns, is still listed, is still chosen and is still paid at its own
  price; what it loses is the ability to route *through* a relay dearer than
  its own pool was sized for, and clients route around that by design.

  `--min-rate` (default: a quarter of `--rate`) now floors the demand
  adjustment, which could previously walk the price down 10 % a window with
  nothing under it. An operator names their cost once instead of watching
  the price.

- **2026-09-09, owner circuits: run a relay, ride it free.** A relay names an
  owner (`--owner`, default `--payout`) and admits a circuit that wallet opens
  on a signature over an owner tag, with no payment. Checked against every
  line above:
  - *Old client, new relay:* nothing changes. The relay adds one hash
    comparison per CREATE; a tag that is not its owner tag takes the exact
    paid path it always took. No wire format, cell, or op changed.
  - *New client, old relay:* a relay listed as `--mine` that predates this
    presents an unknown tag and refuses it the way it refuses any unpaid
    CREATE. The client buys no anchor for an owner attempt, so the refusal
    costs no XNO; it logs why, uses an ordinary entry for that circuit, and
    leaves that relay alone for an hour.
  - *Old app, new Go layer:* the `mine` option is absent, so nothing is
    listed and nothing is tried. *New app, old Go layer:* the field is
    ignored.
  - *Rollback:* an owner tag in `quota.wal` is an ordinary credited tag with
    an owner key; a downgraded relay reads it as one.
  - *Money and routing:* the owner is not paid by anyone. The hops past the
    owner's relay are prepaid from its float, exactly as for every circuit,
    so no other operator carries anything unpaid or earns less. The default
    (`--payout`) grants free circuits only to the key that already receives
    all of the relay's earnings — strictly less than that key already holds.
    Nothing self-reported is involved: the proof is a signature bound to the
    relay's own key.

- **2026-09-09, owner circuits ride at a later hop, never the entry (v0.3.21).**
  A relay this wallet runs is placed as exit or middle, and the owner tag
  travels inside the EXTEND as an optional `tag ‖ sig` after the x25519 key.
  The previous relay forwards it in the CREATE instead of its pool tag,
  prepays nothing and meters nothing for that circuit.
  - *Old relay before ours:* it checks the EXTEND length exactly and answers
    `bad EXTEND`, as it always did for anything it does not understand. The
    client reads that as "no client tags here", remembers it for an hour, and
    pays the ordinary way through that relay. One refused cell per relay per
    hour, no XNO spent, the old relay unchanged.
  - *Old client:* never sends the longer EXTEND; every path is as before.
  - *Old relay as ours:* refuses the tag as an unpaid CREATE; the middle
    reports it as the client's tag and does not touch its own pool; the client
    rests that relay for an hour and pays it like any hop.
  - *Everyone else:* a client with no relays listed sends nothing new and
    chooses paths exactly as before (`TestNothingChangesForAClientWithNoRelays`).
  - *Money:* the entry is paid for its work like any entry; the hop before ours
    spends nothing on ours; ours asks nothing. No operator earns less.
  - *Privacy:* the entry sees an ordinary paying client; an observer of a relay
    we run does not find our address in its inbound; the ledger sees nothing.

- **2026-09-09, three price mechanisms, all per relay, all optional (v0.3.22).**
  - *Cost ledger (client only).* `costs.json` on the device records each anchor
    and its use; `sailnode costs` and the app show it. Nothing on the wire.
  - *Representative-aligned price* (`--rep-friends`, `--rep-bonus`). A relay may
    credit more bytes per XNO when the payer's payment block votes for a listed
    representative. Read from the block the relay already fetches: no extra
    ledger call. Off by default. Changes bytes per XNO only, never routing.
    Old clients get the bonus without knowing; old relays never give one.
  - *Spot price* (`--spot-discount`, `--spot-below`, `--capacity-mbps`). An idle
    relay signs a lower price for a 30-minute window into its own gossip record
    as an optional `spot` field with its **own** signature, outside the record's
    main signature. An old relay verifies the record exactly as before, does not
    know the field, drops it when forwarding, and never refuses the record for
    carrying it. An old client never sees an offer and pays the published price.
    A new client pays the spot price only while the window stands (with two
    minutes of clock slack) and the relay credits payments at the spot price for
    the whole signed window, load or no load. The market median and the price
    cap stay on published prices, so an offer can lower what a client pays but
    never move what "the market asks".
  - *Money:* every mechanism lowers a price only where the relay chose to; no
    relay carries a byte for less than it asked. *Privacy:* the ledger learns
    nothing new; the offer reveals that a relay is idle, which any client could
    measure; the cost ledger never leaves the device.

- **2026-09-09, pairing and the Network switch (v0.3.25).** `sailnode pair`
  prints a six-digit code (five minutes, one use, three tries). The app sends
  it inside an ordinary paid circuit as a new cell, `CmdPair` (30); the relay
  already knows which wallet paid that circuit and, on a match, records it in
  `owners.json` and answers `CmdPaired` (31). Modes: *open* (ours never
  special), *mine* (ours as exit/middle, free there; entry paid), *direct* (one
  hop through ours, nothing paid).
  - *Old relay:* does not know `CmdPair` and says nothing; the client times out
    and says "upgrade this relay to pair it". No other behaviour changes. A
    relay of ours that refuses the owner tag in Direct is rested an hour, as
    for later hops.
  - *Old client / old app:* never sends `CmdPair`, has no switch, behaves as
    v0.3.21 (ours as exit, never entry).
  - *Money:* pairing costs the anchor the app would pay that relay anyway,
    and it stays usable there. Direct pays nobody; nobody else carries a byte.
  - *Privacy:* the code proves shell access to the relay, the circuit proves
    the key; nothing on the ledger. Direct is one hop: the relay (and its
    host) sees the user's address — stated on the switch.

- **2026-09-09, Direct is fast (v0.3.26).** In Direct mode the client sends
  `CmdFast` (32) on circuit 0 and drops the cover cadence, coalescing wait and
  padding on its side; a relay that knows the cell does the same toward that
  client. TLS record cutting is kept, so the link still looks like HTTPS —
  it just no longer keeps an idle browser's rhythm. `--direct-stealth` (the
  app: *Keep the disguise in Direct*) keeps everything as before.
  - *Old relay:* an unknown command on circuit 0 lands in the default branch
    and is ignored (`TestDirectFastCircuitAndUnknownLinkCommandsAreHarmless`
    sends it a command it cannot know); the client is faster, the relay still
    coalesces. Nothing else changes; My relays and Open network are untouched.
  - *Old client:* never sends it.

- **2026-09-09, the fast profile withdrawn (v0.3.28).** Measured on a live
  relay with the relay updated too, interleaved: the disguise on moved 20 MB at
  3.2–4.4 MB/s, "fast" at 1.1–2.2. The cover cadence is a throughput pump, not
  a cost. No client sends `CmdFast` any more; relays ignore it (the number stays
  reserved). Direct uses the standard link. The app applies *Network* and the
  paired list at once by reconnecting itself; before, they were read only at
  the next connect, which looked like the app ignoring the setting.
- **2026-09-10, pairing no longer pays (v0.3.30).** To hand its code to a
  relay the app used to pay that relay an anchor first — proof of work, a
  reachable RPC and a ledger wait on a phone, a minute of "…" that often ended
  in a timeout — and the payment replaced the one the running circuit was made
  with. Now the app opens a *pairing circuit*: CREATE with tag
  `blake2b("sailnet-pairing" ‖ relayPub ‖ walletPub)`, signed by the wallet,
  and `pair:` ‖ walletPub (37 bytes) after the signature where a payment would
  go. A relay honours it only while a code from `sailnode pair` is active
  (five minutes, three tries), for 256 KiB, before any rate limit, and credits
  it to that wallet so `CmdPair` works as before. Relays from before this read
  the trailer as a payment they cannot parse and refuse the CREATE; the app
  then says to run `sailnode pair` for a fresh code and `sailnode upgrade`.
  Nothing is paid to pair any more. Failed attempts are logged on the relay
  (`pairing: wrong code (1 of 3 tries)`), and the app shows the outcome in
  the *My relay* window instead of a toast.
- **2026-09-10, no address leaves the program (v0.3.31, RULES.md rule 6).**
  A pairing attempt against a relay whose port 443 was closed put the dial
  error — with the relay's address — on the phone's screen. Every error the
  mobile boundary returns now passes `client.Redact`, and the pairing error
  says what to do (`ufw allow 443/tcp`) instead of where it failed. Protocol
  unchanged.
- **2026-09-10, a rebooting relay of yours is not a refusing one (v0.3.32).**
  In Direct, a build that failed at the owner's own relay for any reason —
  "no CREATED: EOF" while it restarted after `sailnode upgrade` — was filed as
  "did not accept the owner tag" and rested the relay for an hour, leaving the
  app on "none of your relays is reachable" with both of them up. Only the
  relay's own refusal counts now; no answer skips it for that build only.
  Client-side only.
- **2026-09-10, Direct draws from the paired list (v0.3.33).** The one hop
  of Direct was drawn from the market's candidate set — alive by this
  client's own probe or recent gossip, score ≥ 0.3, under the price cap. A
  phone that had tried its relay while the relay's port was closed had scored
  it out, and Direct reported "none of your relays is reachable" right after
  pairing that very relay had succeeded. The user's own relay is not a
  stranger: Direct now takes it straight from the paired list (still skipped
  for a build that just failed, still rested after its own refusal), nearest
  probed one first. Client-side only.
- **2026-09-10, "connection refused" is not a refusal (v0.3.34).** The v0.3.32
  test for the relay's own refusal matched the word in the kernel's
  `connect: connection refused`, which is what a relay's box says while the
  relay restarts — so `sailnode upgrade` on the operator's relay rested it
  for an hour again. Only `hop 0 refused:` (the relay speaking) counts now.
  Client-side only.
