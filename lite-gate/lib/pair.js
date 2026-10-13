// What the gate does on the relay: log in and run `sailnode pair` (read the
// six digits it prints) or `sailnode unpair`. The host, login and host key
// come from the project's environment; none of them is ever sent to a phone.
//
// The code carries the token's expiry (--until), so the relay itself stops
// carrying the wallet free when the token ends; the app stopping is not the
// only thing that ends it. It is also bound (--wallet) to the one wallet the
// token is bound to, so the six digits pair nobody else. Needs a relay with
// `sailnode pair --until --wallet`: such a relay also prints a line naming
// the wallet and end time it bound the code to, and a code is handed out
// only when that line matches what was asked. A relay too old for the flags
// ignores them and would mint a code that pairs anyone for good; it prints no
// such line, and the gate refuses it.
//
// The same file sits in lite-gate/ (joins) and lite-admin/ (revoking ends
// the relay's free access too). Change both.
const crypto = require('crypto');
const { Client } = require('ssh2');

const WALLET = /^nano_[13][13-9a-km-uw-z]{59}$/;

// runOnRelay runs one sailnode command on the relay and resolves with what it
// printed on stdout and its exit code.
function runOnRelay(args) {
  const { RELAY_SSH_HOST, RELAY_SSH_USER = 'root', RELAY_SSH_PASSWORD, RELAY_SSH_HOSTKEY_SHA256, RELAY_SAIL_HOME = '/var/lib/sailnode' } = process.env;
  if (!RELAY_SSH_HOST || !RELAY_SSH_PASSWORD || !RELAY_SSH_HOSTKEY_SHA256) return Promise.reject(new Error('gate not configured'));
  return new Promise((resolve, reject) => {
    const c = new Client();
    const timer = setTimeout(() => { c.end(); reject(new Error('relay timeout')); }, 25000);
    const done = (err, res) => { clearTimeout(timer); c.end(); err ? reject(err) : resolve(res); };
    c.on('ready', () => {
      c.exec(`SAIL_HOME=${RELAY_SAIL_HOME} /usr/local/bin/sailnode ${args}`, (err, stream) => {
        if (err) return done(err);
        let out = '';
        stream.on('data', (d) => { out += d; });
        stream.stderr.on('data', () => {});
        stream.on('close', (code) => done(null, { out, code }));
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

// parsePairOutput reads `sailnode pair`'s output: the code, provided the
// relay says it bound it to exactly this wallet and end time.
function parsePairOutput(out, until, wallet) {
  const b = out.match(/^Pairing bound: wallet=(\S+) until=(\d+)\s*$/m);
  if (!b || b[1] !== wallet || Number(b[2]) !== until) throw new Error('relay too old: it did not bind the code to the wallet and end time (upgrade sailnode on the relay)');
  const m = out.match(/Pairing code:\s*(\d{3})\s*(\d{3})/);
  if (!m) throw new Error('relay gave no code');
  return m[1] + m[2];
}

async function pairingCode(until, wallet) {
  until = Math.floor(Number(until));
  if (!(until > Date.now() / 1000)) throw new Error('no expiry to pair until');
  if (!WALLET.test(wallet || '')) throw new Error('no wallet to pair');
  const { out } = await runOnRelay(`pair --until ${until} --wallet ${wallet}`);
  return parsePairOutput(out, until, wallet);
}

// unpairWallet ends a wallet's free access on the relay. A wallet that was
// not paired is not an error.
async function unpairWallet(wallet) {
  if (!WALLET.test(wallet || '')) throw new Error('no wallet to unpair');
  const { out, code } = await runOnRelay(`unpair ${wallet}`);
  if (code === 0 && /^(Unpaired\.|That wallet is not paired)/m.test(out)) return;
  throw new Error('relay did not unpair the wallet (is sailnode on the relay too old for `sailnode unpair`?)');
}

module.exports = { pairingCode, unpairWallet, parsePairOutput };
