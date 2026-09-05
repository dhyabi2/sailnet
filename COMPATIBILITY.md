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
