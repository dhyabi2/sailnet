// The operator's side: who may join. Authorization: Bearer ADMIN_SECRET.
//   GET                                   every token (never the token itself)
//   POST {action:"create", label, days}   a new token, shown once
//   POST {action:"revoke"|"restore", id}  switch one off or back on
//   POST {action:"unbind", id}            let it be used on a different phone
//   POST {action:"extend", id, days}      move its expiry, counted from now
const store = require('../lib/store');
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
    if (b.action === 'revoke') change = { revoked: true };
    else if (b.action === 'restore') change = { revoked: false };
    else if (b.action === 'unbind') change = { wallet: '' };
    else if (b.action === 'extend' && days > 0 && days <= 366) change = { expires: Math.floor(Date.now() / 1000) + Math.round(days * 86400) };
    else return res.status(400).json({ ok: false, error: 'unknown action' });
    const rec = await store.update(String(b.id || ''), change);
    if (!rec) return res.status(404).json({ ok: false, error: 'no such token' });
    return res.status(200).json({ ok: true, ...rec });
  } catch (e) {
    console.error('admin:', e.message);
    return res.status(500).json({ ok: false, error: 'storage error' });
  }
};
