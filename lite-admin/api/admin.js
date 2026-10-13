// The operator's side: who may join. Authorization: Bearer ADMIN_SECRET.
//   GET                                   every token (never the token itself)
//   POST {action:"create", label, days}   a new token, shown once
//   POST {action:"revoke"|"restore", id}  switch one off or back on
//   POST {action:"unbind", id}            let it be used on a different phone
//   POST {action:"extend", id, days}      move its expiry, counted from now
//
// Switching a token off, or freeing its phone, also ends that wallet's free
// access on the relay (`sailnode unpair`, over the gate's SSH login, so the
// RELAY_SSH_* variables are needed here too). Extending changes nothing on
// the relay by itself: the phone's next join sees the new expiry and pairs
// again with it (lite-gate/api/join.js).
const store = require('../lib/store');
const { unpairWallet } = require('../lib/pair');
const { body, isAdmin } = require('../lib/http');

module.exports = async (req, res) => {
  if (!isAdmin(req)) return res.status(401).json({ ok: false, error: 'not authorized' });
  try {
    if (req.method === 'GET') {
      const tokens = (await store.all()).map(({ hash, ...t }) => t);
      return res.status(200).json({ ok: true, tokens });
    }
    const b = body(req);
    const days = Number(b.days);
    if (b.action === 'create') {
      if (!(days > 0 && days <= 366)) return res.status(400).json({ ok: false, error: 'days must be between 0 and 366' });
      const { hash, ...t } = await store.create(b.label, days);
      return res.status(200).json({ ok: true, ...t });
    }
    let change;
    // relayUntil: 0 — the relay no longer has an end time from this token,
    // so a token switched back on pairs again on its next join.
    if (b.action === 'revoke') change = { revoked: true, relayUntil: 0 };
    else if (b.action === 'restore') change = { revoked: false };
    else if (b.action === 'unbind') change = { wallet: '', relayUntil: 0 };
    else if (b.action === 'extend' && days > 0 && days <= 366) change = { expires: Math.floor(Date.now() / 1000) + Math.round(days * 86400) };
    else return res.status(400).json({ ok: false, error: 'unknown action' });
    const id = String(b.id || '');
    const before = b.action === 'unbind' ? (await store.all()).find((t) => t.id === id) : null;
    const rec = await store.update(id, change);
    if (!rec) return res.status(404).json({ ok: false, error: 'no such token' });
    const gone = b.action === 'revoke' ? rec.wallet : before ? before.wallet : '';
    if (gone) {
      try {
        await unpairWallet(gone);
      } catch (e) {
        console.error('admin: unpair:', e.message);
        return res.status(502).json({ ok: false, error: `Saved, but the relay did not end this phone's free access (${e.message}). On the relay, run: sailnode unpair ${gone}` });
      }
    }
    return res.status(200).json({ ok: true, ...rec });
  } catch (e) {
    console.error('admin:', e.message);
    return res.status(500).json({ ok: false, error: 'storage error' });
  }
};
