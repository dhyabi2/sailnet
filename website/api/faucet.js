// Sailnet faucet endpoint: https://www.sailnet.space/api/faucet
//
// POST {"account":"nano_..."} → {"ok":true,"hash":...,"amount":"0.0005"}
// The faucet itself runs on a Sailnet relay (FAUCET_UPSTREAM, a Vercel
// environment variable) and pays the registration amount: enough for a
// first circuit or a relay's REGISTER block. This function forwards the
// request with the caller's public IP under a shared secret, so the relay
// can hold the limit of 10 claims per IP per day.
//
// When the faucet cannot pay, the answer is a 503 that names the amount the
// wallet needs, so an app can show "send 0.0005 XNO to <address>" instead
// of a blank failure, and a developer sees the cause in "error".

const https = require("https");
const { URL } = require("url");

const AMOUNT = process.env.FAUCET_AMOUNT || "0.0005";

function forward(body, ip) {
  return new Promise((resolve) => {
    let upstream;
    try {
      upstream = new URL("/faucet", process.env.FAUCET_UPSTREAM);
    } catch (e) {
      return resolve({ status: 503, body: { ok: false, amount: AMOUNT, error: "faucet not configured (FAUCET_UPSTREAM)" } });
    }
    const pin = (process.env.FAUCET_CERT_SHA256 || "").replace(/:/g, "").toUpperCase();
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
        const cert = res.socket.getPeerCertificate();
        const fp = (cert && cert.fingerprint256 ? cert.fingerprint256 : "").replace(/:/g, "").toUpperCase();
        if (pin && fp !== pin) {
          res.resume();
          return resolve({ status: 503, body: { ok: false, amount: AMOUNT, error: "faucet upstream certificate mismatch" } });
        }
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

module.exports = async (req, res) => {
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
  const out = await forward(body, ip);
  res.status(out.status).json(out.body);
};
