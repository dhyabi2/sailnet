// The one thing the gate does on the relay: log in, run `sailnode pair`, and
// read the six digits it prints. The host, login and host key come from the
// project's environment; none of them is ever sent to a phone.
//
// The code carries the token's expiry (--until), so the relay itself stops
// carrying the wallet free when the token ends; the app stopping is not the
// only thing that ends it. It is also bound (--wallet) to the one wallet the
// token is bound to, so the six digits pair nobody else. Needs a relay with
// `sailnode pair --until --wallet`.
const crypto = require('crypto');
const { Client } = require('ssh2');

function pairingCode(until, wallet) {
  until = Math.floor(Number(until));
  if (!(until > Date.now() / 1000)) return Promise.reject(new Error('no expiry to pair until'));
  if (!/^nano_[13][13-9a-km-uw-z]{59}$/.test(wallet || '')) return Promise.reject(new Error('no wallet to pair'));
  const { RELAY_SSH_HOST, RELAY_SSH_USER = 'root', RELAY_SSH_PASSWORD, RELAY_SSH_HOSTKEY_SHA256, RELAY_SAIL_HOME = '/var/lib/sailnode' } = process.env;
  if (!RELAY_SSH_HOST || !RELAY_SSH_PASSWORD || !RELAY_SSH_HOSTKEY_SHA256) return Promise.reject(new Error('gate not configured'));
  return new Promise((resolve, reject) => {
    const c = new Client();
    const timer = setTimeout(() => { c.end(); reject(new Error('relay timeout')); }, 25000);
    const done = (err, code) => { clearTimeout(timer); c.end(); err ? reject(err) : resolve(code); };
    c.on('ready', () => {
      c.exec(`SAIL_HOME=${RELAY_SAIL_HOME} /usr/local/bin/sailnode pair --until ${until} --wallet ${wallet}`, (err, stream) => {
        if (err) return done(err);
        let out = '';
        stream.on('data', (d) => { out += d; });
        stream.stderr.on('data', () => {});
        stream.on('close', () => {
          const m = out.match(/Pairing code:\s*(\d{3})\s*(\d{3})/);
          m ? done(null, m[1] + m[2]) : done(new Error('relay gave no code'));
        });
      });
    });
    c.on('error', (e) => done(e));
    c.connect({
      host: RELAY_SSH_HOST,
      username: RELAY_SSH_USER,
      password: RELAY_SSH_PASSWORD,
      readyTimeout: 15000,
      algorithms: { serverHostKey: ['ssh-ed25519'] },
      // Pinned: the password is only ever offered to the relay we know.
      hostVerifier: (key) => crypto.createHash('sha256').update(key).digest('hex') === RELAY_SSH_HOSTKEY_SHA256.toLowerCase(),
    });
  });
}

module.exports = { pairingCode };
