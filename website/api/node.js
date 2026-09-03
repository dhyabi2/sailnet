// Sailnet's Nano RPC endpoint: https://www.sailnet.space/node/api
//
// Forwards JSON-RPC requests to rpc.nano.to with the Sailnet API key kept
// here (NANO_RPC_KEY, a Vercel environment variable), and falls back to
// public nodes when it fails. Only read/publish/work actions are allowed;
// nothing wallet-related can be reached through it.

const UPSTREAMS = [
  { url: "https://rpc.nano.to", keyed: true },
  { url: "https://rpc.nano-gpt.com" },
  { url: "https://node.somenano.com/proxy" },
  { url: "https://nanoslo.0x.no/proxy" },
  { url: "https://app.natrium.io/api" },
];

const ALLOWED = new Set([
  "account_info", "account_history", "account_balance", "accounts_balances",
  "blocks_info", "block_info", "receivable", "pending", "process",
  "work_generate", "work_validate", "block_count", "representatives_online", "version",
]);

// Per-IP rate limits, ten times what a node or an app needs (measured on the
// live network on 2026-09-04: a client ~8,000 requests/day, a relay ~4,600,
// a home node ~9,200, startup bursts of ~100 in a minute while the registry
// is replayed; work_generate ~57/day, process ~77/day). Buckets live in the
// function instance's memory, so they are per warm instance rather than
// global; upstream limits still apply behind them.
const LIMITS = {
  minute: 600,          // any action, sliding minute
  day: 90000,           // any action, sliding day
  work_generate: 600,   // per day
  process: 800,         // per day
};
const buckets = new Map(); // ip -> { minute: [ts...], day: count, dayStart, work, proc }

function clientIP(req) {
  const xf = req.headers["x-forwarded-for"];
  return (typeof xf === "string" ? xf.split(",")[0] : "") || req.socket?.remoteAddress || "unknown";
}

function limited(ip, action) {
  const now = Date.now();
  let b = buckets.get(ip);
  if (!b || now - b.dayStart > 86400000) { b = { minute: [], day: 0, dayStart: now, work: 0, proc: 0 }; buckets.set(ip, b); }
  b.minute = b.minute.filter(t => now - t < 60000);
  if (b.minute.length >= LIMITS.minute) return "minute";
  if (b.day >= LIMITS.day) return "day";
  if (action === "work_generate" && b.work >= LIMITS.work_generate) return "work_generate";
  if (action === "process" && b.proc >= LIMITS.process) return "process";
  b.minute.push(now); b.day++;
  if (action === "work_generate") b.work++;
  if (action === "process") b.proc++;
  if (buckets.size > 20000) buckets.clear(); // memory guard
  return "";
}

export default async function handler(req, res) {
  res.setHeader("Access-Control-Allow-Origin", "*");
  res.setHeader("Access-Control-Allow-Headers", "Content-Type");
  if (req.method === "OPTIONS") return res.status(204).end();
  if (req.method !== "POST") return res.status(405).json({ error: "POST only" });
  let body = req.body;
  if (typeof body === "string") { try { body = JSON.parse(body); } catch { body = null; } }
  if (!body || typeof body.action !== "string") return res.status(400).json({ error: "Action not provided" });
  if (!ALLOWED.has(body.action)) return res.status(403).json({ error: "Forbidden command" });
  const why = limited(clientIP(req), body.action);
  if (why) {
    res.setHeader("Retry-After", why === "minute" ? "60" : "3600");
    return res.status(429).json({ error: "rate limit", limit: why, retry_after: why === "minute" ? 60 : 3600 });
  }
  delete body.key;
  const key = process.env.NANO_RPC_KEY || "";
  let last = { status: 502, text: "no upstream answered" };
  for (const up of UPSTREAMS) {
    const payload = { ...body };
    const headers = { "Content-Type": "application/json", "nano-app": "sailnet" };
    if (up.keyed && key) { payload.key = key; headers.key = key; }
    try {
      const ctl = new AbortController();
      const t = setTimeout(() => ctl.abort(), body.action === "work_generate" ? 90000 : 20000);
      const r = await fetch(up.url, { method: "POST", headers, body: JSON.stringify(payload), signal: ctl.signal });
      clearTimeout(t);
      const text = await r.text();
      if (r.status === 429 || r.status >= 500) { last = { status: r.status, text }; continue; }
      let json; try { json = JSON.parse(text); } catch { last = { status: 502, text }; continue; }
      const err = typeof json.error === "string" ? json.error.toLowerCase() : "";
      if (json.error !== undefined && (err.includes("limit") || err.includes("banned") || err.includes("unauthorized"))) { last = { status: 429, text }; continue; }
      res.setHeader("X-Sailnet-Upstream", new URL(up.url).host);
      return res.status(200).send(text);
    } catch (e) {
      last = { status: 502, text: String(e) };
    }
  }
  return res.status(last.status).json({ error: "all upstreams failed", detail: last.text.slice(0, 200) });
}
