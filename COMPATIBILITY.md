# Strict rule: the network is live. Never break a running node.

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
