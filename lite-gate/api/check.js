// POST {token, wallet} — is this phone still allowed? Asked before every
// connect and now and then while connected. 403 means "stop"; anything else
// that is not 200 means "could not ask", and the app keeps going.
const store = require('../lib/store');
const { body } = require('../lib/http');

module.exports = async (req, res) => {
  if (req.method !== 'POST') return res.status(405).json({ ok: false, error: 'POST only' });
  const { token, wallet } = body(req);
  try {
    const { rec, error } = await store.admit(token, wallet);
    if (error) return res.status(403).json({ ok: false, error });
    return res.status(200).json({ ok: true, expires: rec.expires });
  } catch (e) {
    console.error('check:', e.message);
    return res.status(503).json({ ok: false, error: 'The service is busy.' });
  }
};
