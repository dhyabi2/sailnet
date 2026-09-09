# Standing rules

The network is live and run by strangers. Six rules decide whether a change
ships. They are not goals to weigh against others; a change that fails any of
them does not ship, whatever it improves.

---

## 1. Never break a node that does not upgrade

Full text and checklist: `COMPATIBILITY.md`.

> Does a node that never upgrades keep working, keep being chosen, and keep
> earning exactly as before?

---

## 2. No centralization

Nothing in the running network may depend on a party everyone has to trust,
ask, or wait for.

**Forbidden**

- A service that decides which relays exist, which are good, or what anything
  costs. Every such judgement is made by each client and each relay, from its
  own measurements and from public ledger data.
- A registry, tracker, directory or bootstrap server that the network stops
  working without.
- A key that can remove a relay, change a price, or reorder demand.
- Anything that makes our machines special in the protocol. Our relays run the
  published binary with the published flags, and are chosen by the same draw
  as anyone else's.

**Allowed, and how it must be built**

- A convenience that has a working path when it disappears. Today: the Nano
  RPC endpoint (falls back to public nodes and to the entry relay), the
  faucet (a wallet can be funded by hand; the message says how much and
  where), the bridge list (falls back to listed relays), the website and the
  release page (existing installs keep working without them).
- The treasury address is a rendezvous, not an authority: registrations are
  sends *to* it that anyone can publish and nobody can withdraw, censor or
  reorder. Losing its key changes nothing about who is listed.

**Test for any new dependency:** turn it off and describe what a user loses.
If the answer is "the network", it does not ship.

---

## 3. No central custody

Nobody ever holds anyone else's money.

- A relay's earnings arrive in a wallet whose seed exists only on that
  machine, and are swept only to an address its operator chose.
- A client's XNO sits in a wallet whose seed exists only on that device.
- Payment is a direct send from payer to payee on the public ledger. There is
  no escrow, no pool account, no balance we keep on someone's behalf, no
  credit anyone extends.
- Prepayment is the only money that leaves before service: a client prepays
  its entry, a relay prepays the next hop. It is bounded, it is the payer's
  own decision, and it buys a metered quota at one named counterparty.
- We never ask for a seed, never generate a wallet a user does not control,
  and never route a payout through an address of ours.

**Test:** if we vanished tonight, could every operator still spend everything
they earned? The answer must be yes, with no action from us.

---

## 4. Anti-manipulation

Assume every published number is written by someone who profits from it.
Nothing self-reported may decide money or routing on its own.

**Rules**

- **Registration is a claim, not a fact.** It costs one raw, so treat the
  register as an open noticeboard. Anything that spends money, sends traffic,
  or sets a price must first have proof the relay is there: a connection that
  opens, or a signed record that arrived through gossip.
- **Never let unverified records move a market number.** Prices, medians,
  weights and counts are computed over relays proven to be there. A thousand
  forged cheap records must not shift the median (see
  `TestFakeCheapRegistrationsCannotPriceOutRealRelays`).
- **Never pay on a claim.** Pools are prepaid only to relays that answer.
- **Pay for delivery, not for assertion.** Reward weight comes from bytes
  actually carried on circuits the payer built, never from a relay's own
  statistics.
- **Cap what a single party can win.** Price caps, per-path country and ASN
  diversity, per-IP limits, and a weighted random draw instead of "best wins",
  so buying the top score buys nothing.
- **Randomness where an attacker would want determinism.** Path choice is a
  weighted draw; being fastest or cheapest must never guarantee selection.
- **Make the attack cost more than the prize.** Before shipping a mechanism,
  write down what an attacker gains by lying to it and what it costs them.

**Test for any new signal:** who publishes it, what do they gain by lying,
and what does the code do if they do?

---

## 5. Never build or release from this repository

This repository is private history and staging. **Every artifact anyone can
download is built and published from the public mirror, `dhyabi2/sailnet`,
and from nowhere else.**

**Forbidden here**

- Pushing a `v*` tag. Tagging is what cuts a release, and this repository must
  never cut one.
- Enabling, dispatching or re-adding a build workflow. All five release
  workflows are disabled on GitHub, their triggers reduced to
  `workflow_dispatch`, and every job fenced behind
  `if: github.repository == 'dhyabi2/sailnet'`. Three locks, because one is
  what we had.
- Publishing a release, or attaching an artifact to one, by hand.

**Why**

`sailnode upgrade` reads `repos/dhyabi2/sailnet/releases/latest`, the website
links there, and every download URL points there. A second publisher does not
add a channel, it splits the truth:

- **Version numbers stop meaning anything.** This repository reached v0.3.16
  while the public mirror was at v0.2.62 — a higher number on older, private
  code. An operator comparing them concludes the real release is behind.
- **The Android build becomes uninstallable.** `versionCode` is
  `git rev-list --count`, a property of the repository rather than of the
  product, and this history is longer. A private APK therefore outranks every
  real release and Android refuses the real one as a downgrade — and the
  private build is `-universal-debug`, signed with the debug key, so it will
  not be replaced by a properly signed release at all.

The mirror is curated, not copied: it omits `RULES.md`, `archive/`, `deploy/`
(wallets, secrets, VPS addresses) and `docs/`, and scrubs comments that name
them. So a release built here would also ship what the scrubbing exists to
keep out.

**Test:** does the artifact a stranger downloads come from a tag on
`dhyabi2/sailnet`? If it came from anywhere else, it does not ship.

---

## Where these came from

- 2026-09-05: 34 of 42 registry records were forged or dead, several naming
  Tor exit addresses. Our relays had prepaid a pool to almost all of them.
  Rules 2 and 4 are written from that.
- 2026-09-08: this repository had been publishing releases too — fifteen of
  them, up to v0.3.14, alongside the public v0.2.62. Rule 5 is written from
  that.

---

## 6. No address leaves the program

No IP address — the user's, a relay's, anyone's — appears in anything a
person can see: not in the app, the desktop window, an error message, a
toast, a log line, a support bundle or a crash report. An address is
identity; a screenshot of an error must not be a map of who talked to whom.

**How it must be built**

- Every string that crosses into a UI, a returned error or a log passes
  through `client.Redact` (`sail/client/redact.go`). The mobile boundary
  (`sail/mobile`) redacts every error it returns; the log writers are
  `RedactingWriter`. A new boundary gets the same wrapper before it ships.
- Errors from dialing, TLS or HTTP are never returned as they came: they
  name the peer. Wrap them in a message that says what to do, not where.
- Relays are named by country and short account, never by address; the
  user by nickname or "user", never by address.
- A test that finds an address in a UI string or a log fails the build.
