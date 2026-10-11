// Tokens live in a private Vercel Blob store, one small JSON file each, named
// by the SHA-256 of the token: the gate never keeps a token it could leak.
//
//   { id, label, created, expires, wallet, revoked }
//
// id is the first 12 hex of the hash (what the admin page shows), expires a
// Unix time, wallet the Nano address of the one phone the token is bound to
// (set the first time it is used).
//
// The same file sits in lite-gate/ (the phones) and lite-admin/ (the operator):
// two Vercel apps over one store. Change both.
const crypto = require('crypto');
const { put, get, list } = require('@vercel/blob');

const ALPHABET = 'ABCDEFGHJKMNPQRSTUVWXYZ23456789'; // no 0/O, 1/I/L: it is typed on a phone

function normalize(token) {
  return String(token || '').toUpperCase().replace(/[^A-Z0-9]/g, '');
}

function hash(token) {
  return crypto.createHash('sha256').update(normalize(token)).digest('hex');
}

function newToken() {
  let s = '';
  for (const b of crypto.randomBytes(12)) s += ALPHABET[b % ALPHABET.length];
  return s.match(/.{4}/g).join('-');
}

const path = (h) => `tokens/${h}.json`;

async function readPath(p) {
  const r = await get(p, { access: 'private', useCache: false });
  if (!r || r.statusCode !== 200) return null;
  return JSON.parse(await new Response(r.stream).text());
}

async function write(h, rec) {
  await put(path(h), JSON.stringify(rec), {
    access: 'private',
    addRandomSuffix: false,
    allowOverwrite: true,
    contentType: 'application/json',
  });
}

async function create(label, days) {
  const token = newToken();
  const h = hash(token);
  const now = Math.floor(Date.now() / 1000);
  const rec = { id: h.slice(0, 12), label: String(label || '').slice(0, 60), created: now, expires: now + Math.round(days * 86400), wallet: '', revoked: false };
  await write(h, rec);
  return { token, ...rec };
}

async function all() {
  const out = [];
  let cursor;
  do {
    const page = await list({ prefix: 'tokens/', cursor });
    for (const b of page.blobs) {
      const rec = await readPath(b.pathname);
      if (rec) out.push({ ...rec, hash: b.pathname.slice('tokens/'.length, -'.json'.length) });
    }
    cursor = page.cursor;
  } while (cursor);
  return out.sort((a, b) => b.created - a.created);
}

// admit says whether this token lets this wallet in, binding the token to
// the wallet on first use. Returns { rec } or { error }.
async function admit(token, wallet) {
  if (normalize(token).length !== 12) return { error: 'That code is not valid.' };
  if (!/^nano_[13][13-9a-km-uw-z]{59}$/.test(wallet || '')) return { error: 'This app could not identify itself. Reinstall it and try again.' };
  const h = hash(token);
  const rec = await readPath(path(h));
  if (!rec) return { error: 'That code is not valid.' };
  if (rec.revoked) return { error: 'This code has been switched off.' };
  if (rec.expires <= Date.now() / 1000) return { error: 'This code has expired.' };
  if (rec.wallet && rec.wallet !== wallet) return { error: 'This code is already in use on another phone.' };
  if (!rec.wallet) {
    rec.wallet = wallet;
    await write(h, rec);
  }
  return { rec };
}

// update changes one token found by its id (admin page).
async function update(id, change) {
  for (const rec of await all()) {
    if (rec.id !== id) continue;
    const { hash: h, ...rest } = rec;
    const next = { ...rest, ...change };
    await write(h, next);
    return next;
  }
  return null;
}

module.exports = { create, all, admit, update };
