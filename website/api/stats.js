// Sailnet network stats: https://www.sailnet.space/api/stats
//
// Every relay answers a coarse summary of what it sees on the ledger at
// /stats. This function asks a few of them and returns the first answer, so
// the page shows the network's size without anyone having to trust a number
// we keep ourselves. The relays to ask come from STATS_RELAYS (a Vercel
// environment variable, comma-separated https://host); if it is unset or all
// of them are unreachable, the page simply says the figures are unavailable.

import https from "https";
import { URL } from "url";

const CACHE_SECONDS = 60;
let cached = null;
let cachedAt = 0;

function ask(base) {
  return new Promise((resolve) => {
    let u;
    try {
      u = new URL("/stats", base.trim());
    } catch (e) {
      return resolve(null);
    }
    const req = https.request(
      {
        method: "GET",
        host: u.hostname,
        port: u.port || 443,
        path: u.pathname,
        // A relay serves a certificate for its decoy hostname, not for its
        // address; the figures here are public and unsigned either way.
        rejectUnauthorized: false,
        servername: u.hostname,
        timeout: 8000,
      },
      (res) => {
        let data = "";
        res.on("data", (c) => (data += c));
        res.on("end", () => {
          try {
            const j = JSON.parse(data);
            resolve(j && typeof j.registered === "number" ? j : null);
          } catch (e) {
            resolve(null);
          }
        });
      }
    );
    req.on("timeout", () => req.destroy(new Error("timeout")));
    req.on("error", () => resolve(null));
    req.end();
  });
}

export default async function handler(req, res) {
  res.setHeader("Access-Control-Allow-Origin", "*");
  res.setHeader("Cache-Control", "public, max-age=30, s-maxage=60");
  if (req.method === "OPTIONS") return res.status(204).end();

  if (cached && Date.now() - cachedAt < CACHE_SECONDS * 1000) {
    return res.status(200).json(cached);
  }
  const relays = (process.env.STATS_RELAYS || "").split(",").filter(Boolean);
  if (relays.length === 0) {
    return res.status(503).json({ ok: false, error: "no relays configured to ask" });
  }
  const answers = (await Promise.all(relays.map(ask))).filter(Boolean);
  if (answers.length === 0) {
    return res.status(503).json({ ok: false, error: "no relay answered" });
  }
  // Take the widest view: relays disagree only by how much gossip each has
  // seen, and the fullest picture is the most useful one.
  const best = answers.reduce((a, b) => (b.alive > a.alive ? b : a));
  const out = {
    ok: true,
    registered: best.registered,
    alive: best.alive,
    exits: best.exits,
    countries: best.countries || [],
    priceXnoPerMiB: best.priceXnoPerMiB,
    relayedMiB: answers.reduce((n, a) => n + (a.relayedMiB || 0), 0),
    circuits: answers.reduce((n, a) => n + (a.circuits || 0), 0),
    asked: relays.length,
    answered: answers.length,
  };
  cached = out;
  cachedAt = Date.now();
  res.status(200).json(out);
}
