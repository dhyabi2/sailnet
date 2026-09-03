const nano = require('nanocurrency');
const fs = require('fs');
const path = require('path');
const out = path.join(__dirname, '..', '.wallet', 'treasury.json');
if (fs.existsSync(out)) { const w = JSON.parse(fs.readFileSync(out)); console.log(w.address); process.exit(0); }
const seed = nano.generateSeed ? null : null;
(async () => {
  const s = await nano.generateSeed();
  const priv = nano.deriveSecretKey(s, 0);
  const pub = nano.derivePublicKey(priv);
  const address = nano.deriveAddress(pub, { useNanoPrefix: true });
  fs.writeFileSync(out, JSON.stringify({ seed: s, index: 0, address, created: new Date().toISOString() }, null, 2), { mode: 0o600 });
  console.log(address);
})();
