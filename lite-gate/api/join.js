// POST {token, wallet, needCode} — the app's Connect button.
// A valid token gets the relay to pair with and, when the phone has not
// paired yet, a fresh pairing code minted on that relay.
const store = require('../lib/store');
const { pairingCode } = require('../lib/pair');
const { body } = require('../lib/http');

module.exports = async (req, res) => {
  if (req.method !== 'POST') return res.status(405).json({ ok: false, error: 'POST only' });
  const { token, wallet, needCode } = body(req);
  try {
    const { rec, error } = await store.admit(token, wallet);
    if (error) return res.status(403).json({ ok: false, error });
    const out = { ok: true, account: process.env.RELAY_ACCOUNT, expires: rec.expires };
    if (needCode) out.code = await pairingCode();
    return res.status(200).json(out);
  } catch (e) {
    console.error('join:', e.message);
    return res.status(503).json({ ok: false, error: 'The service is busy. Try again in a minute.' });
  }
};
