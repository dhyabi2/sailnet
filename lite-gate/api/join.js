// POST {token, wallet, needCode, repair} — the app's Connect button.
// A valid token gets the relay to pair with and, when the phone has not
// paired yet, a fresh pairing code minted on that relay. An app that sends
// repair: true (it pairs again with any code it is handed) also gets a code
// when the relay's end time for it is out of date (the operator extended the
// token, or switched it back on); an older app, which would ignore such a
// code, never gets one it did not ask for.
const store = require('../lib/store');
const { pairingCode, unpairWallet } = require('../lib/pair');
const { body } = require('../lib/http');

module.exports = async (req, res) => {
  if (req.method !== 'POST') return res.status(405).json({ ok: false, error: 'POST only' });
  const { token, wallet, needCode, repair } = body(req);
  try {
    const { rec, error } = await store.admit(token, wallet);
    if (error) return res.status(403).json({ ok: false, error });
    const out = { ok: true, account: process.env.RELAY_ACCOUNT, expires: rec.expires };
    const stale = repair === true && rec.relayUntil !== rec.expires;
    if (needCode || stale) {
      let code = '';
      try {
        code = await pairingCode(rec.expires, rec.wallet); // ends when the token does; pairs only the token's wallet
      } catch (e) {
        if (needCode) throw e;
        // Already paired: it keeps riding on the end time the relay has, and
        // relayUntil stays as it was, so the next join tries again.
        console.error('join: re-pair:', e.message);
      }
      if (code) {
        // Switched off or freed while the code was being minted (lite-admin
        // unpairs then): hand nothing out, and end what the relay has.
        const now = await store.lookup(token);
        if (!now || now.revoked || now.wallet !== rec.wallet) {
          try { await unpairWallet(rec.wallet); } catch (e) { console.error('join: unpair:', e.message); }
          return res.status(403).json({ ok: false, error: !now || now.revoked ? 'This code has been switched off.' : 'This code is already in use on another phone.' });
        }
        out.code = code;
        // Recorded only for an app that re-pairs on a code (repair), and only
        // if the expiry is still the one the code carries.
        if (repair === true) await store.setRelayUntil(token, rec.expires, rec.wallet);
      }
    }
    return res.status(200).json(out);
  } catch (e) {
    console.error('join:', e.message);
    return res.status(503).json({ ok: false, error: 'The service is busy. Try again in a minute.' });
  }
};
