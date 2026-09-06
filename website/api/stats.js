// Sailnet network stats: https://www.sailnet.space/api/stats
//
// Every relay answers a coarse summary of what it sees on the ledger at
// /stats. This function asks several of them at once and merges the answers,
// so the page shows the network's size without anyone having to trust a
// number we keep ourselves. The relays to ask come from STATS_RELAYS (a
// Vercel environment variable, comma-separated https://host).
//
// Robustness is the point of most of what follows. The figures are a nicety
// on a marketing page — the network does not depend on them — so this
// endpoint should never be the reason the page looks broken:
//
//   - answers are cached in the instance, in /tmp, and at Vercel's edge with
//     stale-while-revalidate, which is the layer that survives a cold start
//     or a redeploy;
//   - when no relay answers, the last good figures are returned with
//     stale:true and the time they were taken, instead of an error;
//   - only when nothing has ever been cached does it admit it has nothing.

import https from "https";
import fs from "fs";
import path from "path";
import os from "os";
import { URL } from "url";

const FRESH_MS = 60_000; // how long an answer is considered current
const ASK_TIMEOUT_MS = 6_000; // a slow relay must not hold up the page
const DISK = path.join(os.tmpdir(), "sailnet-stats.json");

let memory = null; // { at: epoch_ms, data: {...} }

function loadCache() {
  if (memory) return memory;
  try {
    const raw = JSON.parse(fs.readFileSync(DISK, "utf8"));
    if (raw && raw.at && raw.data) memory = raw;
  } catch (e) {
    /* no cache yet, or unreadable: ask the relays */
  }
  return memory;
}

function saveCache(data) {
  memory = { at: Date.now(), data };
  try {
    fs.writeFileSync(DISK, JSON.stringify(memory));
  } catch (e) {
    /* a read-only or full /tmp costs us the cross-invocation cache, nothing more */
  }
  return memory;
}

// ask one relay for its view of the network.
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
        // An SNI name may not be an IP address (RFC 6066), and relays are
        // usually addressed by one; sending it anyway is deprecated in Node.
        servername: /^[0-9.]+$|:/.test(u.hostname) ? undefined : u.hostname,
        timeout: ASK_TIMEOUT_MS,
      },
      (res) => {
        let data = "";
        res.on("data", (c) => {
          data += c;
          if (data.length > 1 << 20) req.destroy(); // a relay cannot flood this function
        });
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

// merge takes the widest view. Relays disagree only by how much gossip each
// has seen, and the fullest picture is the most useful one; traffic counters
// are per-relay, so those add up.
function merge(answers, asked) {
  const out = {};
  const best = answers.reduce((a, b) => (b.alive > a.alive ? b : a));
  const countries = [...new Set(answers.flatMap((a) => a.countries || []))].sort();
  Object.assign(out, {
    ok: true,
    registered: best.registered,
    alive: best.alive,
    exits: best.exits,
    countries,
    priceXnoPerMiB: best.priceXnoPerMiB,
    relayedMiB: answers.reduce((n, a) => n + (a.relayedMiB || 0), 0),
    circuits: answers.reduce((n, a) => n + (a.circuits || 0), 0),
    asked,
    answered: answers.length,
  });
  // Only relays running a faucet report on one, so these are absent from
  // most answers and are left out entirely when nobody reported them —
  // showing "0 free trials" because we happened not to ask the right relay
  // would be worse than showing nothing.
  const withFaucet = answers.filter((a) => typeof a.faucetPaidToday === "number");
  if (withFaucet.length > 0) {
    out.faucetPaidToday = withFaucet.reduce((n, a) => n + a.faucetPaidToday, 0);
    out.faucetRefusedToday = withFaucet.reduce((n, a) => n + (a.faucetRefusedToday || 0), 0);
    const last = withFaucet.map((a) => a.faucetLastPaid).filter(Boolean).sort();
    if (last.length > 0) out.faucetLastPaid = last[last.length - 1];
  }
  return out;
}

export default async function handler(req, res) {
  res.setHeader("Access-Control-Allow-Origin", "*");
  // Two separate caches, told apart deliberately. The browser holds a copy
  // for a few seconds so a reload is instant; Vercel's edge holds one for a
  // minute and will keep serving it for a day while it refreshes behind the
  // reader's back, which is the layer that survives a cold start, a redeploy
  // or a relay going quiet. The edge only honours its own header — a plain
  // s-maxage is dropped — so it gets one of its own.
  res.setHeader("Cache-Control", "public, max-age=15");
  res.setHeader("Vercel-CDN-Cache-Control", "max-age=60, stale-while-revalidate=86400");
  res.setHeader("CDN-Cache-Control", "max-age=60, stale-while-revalidate=86400");
  if (req.method === "OPTIONS") return res.status(204).end();

  const cached = loadCache();
  if (cached && Date.now() - cached.at < FRESH_MS) {
    return res.status(200).json({ ...cached.data, asOf: new Date(cached.at).toISOString(), stale: false });
  }

  const relays = (process.env.STATS_RELAYS || "").split(",").filter(Boolean);
  let answers = [];
  if (relays.length > 0) {
    answers = (await Promise.all(relays.map(ask))).filter(Boolean);
  }

  if (answers.length > 0) {
    const fresh = saveCache(merge(answers, relays.length));
    return res.status(200).json({ ...fresh.data, asOf: new Date(fresh.at).toISOString(), stale: false });
  }

  // Nothing answered. Old figures, honestly labelled, beat an error on a page
  // whose job is to describe a network that is still running perfectly well.
  if (cached) {
    return res.status(200).json({
      ...cached.data,
      asOf: new Date(cached.at).toISOString(),
      stale: true,
      note: "no relay answered just now; these are the last figures we have",
    });
  }
  return res.status(503).json({
    ok: false,
    error: relays.length === 0 ? "no relays configured to ask" : "no relay answered",
  });
}
