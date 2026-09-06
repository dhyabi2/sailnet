// Sailnet faucet endpoint: https://www.sailnet.space/api/faucet
//
// POST {"account":"nano_..."} → {"ok":true,"hash":...,"amount":"0.0005"}
// The faucet itself runs on Sailnet relays (FAUCET_UPSTREAM, a Vercel
// environment variable, one host or several separated by commas) and pays
// the registration amount: enough for a first circuit or a relay's REGISTER
// block. This function forwards the request with the caller's public IP
// under a shared secret, so the relay can hold the per-IP daily limit.
//
// With more than one host it tries them in order and moves on when a faucet
// is unreachable or broken — including when its wallet has run dry, which is
// a 503. It does not move on when a faucet answers 4xx: a refusal is a real
// answer, and retrying it elsewhere would quietly double the daily limit
// every rate limit is there to hold.
//
// When the faucet cannot pay, the answer is a 503 that names the amount the
// wallet needs, so an app can show "send 0.0005 XNO to <address>" instead
// of a blank failure, and a developer sees the cause in "error".

import https from "https";
import { URL } from "url";

const AMOUNT = process.env.FAUCET_AMOUNT || "0.0005";

function forward(body, ip, host) {
  return new Promise((resolve) => {
    let upstream;
    try {
      upstream = new URL("/faucet", host);
    } catch (e) {
      return resolve({ status: 503, body: { ok: false, amount: AMOUNT, error: "faucet not configured (FAUCET_UPSTREAM)" } });
    }
    const req = https.request(
      {
        method: "POST",
        host: upstream.hostname,
        port: upstream.port || 443,
        path: upstream.pathname,
        headers: { "content-type": "application/json", "x-faucet-secret": process.env.FAUCET_SECRET || "", "x-forwarded-for": ip },
        rejectUnauthorized: false,
        servername: upstream.hostname,
        timeout: 120000,
      },
      (res) => {
        // The relay's certificate is self-signed and depends on the SNI it
        // sees, so it is not pinned here; the shared secret header is what
        // authorises the forwarded claim, and the account and address in it
        // are public data anyway.
        let data = "";
        res.on("data", (c) => (data += c));
        res.on("end", () => {
          try {
            resolve({ status: res.statusCode, body: JSON.parse(data) });
          } catch (e) {
            resolve({ status: 503, body: { ok: false, amount: AMOUNT, error: "faucet upstream returned an unreadable answer (HTTP " + res.statusCode + ")" } });
          }
        });
      }
    );
    req.on("timeout", () => req.destroy(new Error("timeout")));
    req.on("error", (e) => resolve({ status: 503, body: { ok: false, amount: AMOUNT, error: "faucet unreachable (" + e.message + "): the registration amount of " + AMOUNT + " XNO must be sent to the wallet by hand" } }));
    req.end(body);
  });
}

export default async function handler(req, res) {
  res.setHeader("Access-Control-Allow-Origin", "*");
  res.setHeader("Access-Control-Allow-Headers", "content-type");
  if (req.method === "OPTIONS") return res.status(204).end();
  if (req.method !== "POST") {
    return res.status(405).json({ ok: false, amount: AMOUNT, error: 'POST {"account":"nano_..."}' });
  }
  const ip = ((req.headers["x-forwarded-for"] || req.headers["x-real-ip"] || req.socket.remoteAddress || "") + "").split(",")[0].trim();
  let body = "";
  if (typeof req.body === "string") body = req.body;
  else if (req.body && typeof req.body === "object") body = JSON.stringify(req.body);
  if (!body) {
    return res.status(400).json({ ok: false, amount: AMOUNT, error: "missing body" });
  }
  const hosts = (process.env.FAUCET_UPSTREAM || "").split(",").map((h) => h.trim()).filter(Boolean);
  if (hosts.length === 0) {
    return res.status(503).json({ ok: false, amount: AMOUNT, error: "faucet not configured (FAUCET_UPSTREAM)" });
  }
  let out;
  for (const host of hosts) {
    out = await forward(body, ip, host);
    if (out.status < 500) break; // an answer, including a refusal
  }
  res.status(out.status).json(out.body);
}
