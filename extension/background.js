// Sailnet browser extension.
//
// Fail-safe by construction: the proxy is applied only while two things are
// true at the same time, the user wants it ("enabled") and the local client
// is actually answering on its status port. When either stops being true the
// proxy and the WebRTC policy are cleared, so a browser is never left pointing
// at a dead port after the client quits, the machine reboots, or the
// extension is idle. There is no blocking mode: the browser always has a
// working network, through Sailnet when it runs, directly otherwise.

const DEFAULTS = { socksHost: "127.0.0.1", socksPort: 1080, statusUrl: "http://127.0.0.1:1090", enabled: false };

// applied: what the browser is configured to right now ("direct" | "proxy" | "blocked").
// Kept in storage because the service worker is stopped between events.
const STATE_KEY = "applied";

async function settings() {
  return { ...DEFAULTS, ...(await chrome.storage.local.get(Object.keys(DEFAULTS))) };
}

const isFirefox = typeof navigator !== "undefined" && /Firefox/.test(navigator.userAgent);

async function setProxy(s) {
  if (isFirefox) {
    await chrome.proxy.settings.set({
      value: { proxyType: "manual", socks: s.socksHost + ":" + Number(s.socksPort), socksVersion: 5, proxyDNS: true, passthrough: "localhost, 127.0.0.1" },
      scope: "regular"
    });
  } else {
    await chrome.proxy.settings.set({
      value: {
        mode: "fixed_servers",
        rules: { singleProxy: { scheme: "socks5", host: s.socksHost, port: Number(s.socksPort) }, bypassList: ["127.0.0.1", "localhost", "<-loopback>"] }
      },
      scope: "regular"
    });
  }
  try { await chrome.privacy.network.webRTCIPHandlingPolicy.set({ value: "disable_non_proxied_udp" }); } catch (e) {}
}

async function setDirect() {
  await chrome.proxy.settings.clear({ scope: "regular" });
  try { await chrome.privacy.network.webRTCIPHandlingPolicy.clear({}); } catch (e) {}
}

async function status() {
  const s = await settings();
  const ctl = new AbortController();
  const t = setTimeout(() => ctl.abort(), 1500);
  try {
    const r = await fetch(s.statusUrl + "/status", { cache: "no-store", headers: { "X-Sail": "1" }, signal: ctl.signal });
    if (!r.ok) throw new Error("http " + r.status);
    return await r.json();
  } catch (e) {
    return null;
  } finally {
    clearTimeout(t);
  }
}

// reconcile is the whole state machine. Idempotent; safe to call often.
let reconciling = null;
async function reconcile() {
  if (reconciling) return reconciling;
  reconciling = (async () => {
    const s = await settings();
    const st = s.enabled ? await status() : null;
    const want = s.enabled && st ? "proxy" : "direct";
    const { [STATE_KEY]: applied } = await chrome.storage.local.get(STATE_KEY);
    if (want !== applied) {
      if (want === "proxy") await setProxy(s);
      else await setDirect();
      await chrome.storage.local.set({ [STATE_KEY]: want });
    }
    await badge(s, st, want);
    return { settings: s, status: st, applied: want };
  })();
  try { return await reconciling; } finally { reconciling = null; }
}

async function badge(s, st, applied) {
  if (!s.enabled) {
    await chrome.action.setBadgeText({ text: "" });
    await chrome.action.setTitle({ title: "Sailnet: off" });
    return;
  }
  if (st && st.path) {
    await chrome.action.setBadgeBackgroundColor({ color: "#111111" });
    await chrome.action.setBadgeText({ text: "ON" });
    await chrome.action.setTitle({ title: "Sailnet: " + st.path });
  } else if (st) {
    await chrome.action.setBadgeBackgroundColor({ color: "#666666" });
    await chrome.action.setBadgeText({ text: "…" });
    await chrome.action.setTitle({ title: st.needsFunds ? "Sailnet: waiting for XNO" : "Sailnet: building circuit" });
  } else {
    await chrome.action.setBadgeBackgroundColor({ color: "#B3731A" });
    await chrome.action.setBadgeText({ text: "!" });
    await chrome.action.setTitle({ title: "Sailnet: client not running; browsing directly" });
  }
}

// Wake-ups: install, browser start, a periodic alarm (the floor MV3 allows),
// and, when enabled, every failed proxy connection: the browser tells us the
// instant the client is gone, no polling delay.
chrome.runtime.onInstalled.addListener(async () => {
  await chrome.alarms.create("tick", { periodInMinutes: 0.5 });
  reconcile();
});
chrome.runtime.onStartup.addListener(async () => {
  await chrome.alarms.create("tick", { periodInMinutes: 0.5 });
  reconcile();
});
chrome.alarms.onAlarm.addListener(a => { if (a.name === "tick") reconcile(); });

if (chrome.webRequest && chrome.webRequest.onErrorOccurred) {
  let last = 0;
  chrome.webRequest.onErrorOccurred.addListener(d => {
    if (!/PROXY|SOCKS/i.test(d.error || "")) return;
    const now = Date.now();
    if (now - last < 2000) return; // one check per burst of failures
    last = now;
    reconcile();
  }, { urls: ["<all_urls>"] });
}

chrome.runtime.onMessage.addListener((msg, _sender, reply) => {
  (async () => {
    if (msg.type === "toggle") {
      await chrome.storage.local.set({ enabled: msg.enabled });
      reply(await reconcile());
    } else if (msg.type === "status") {
      reply(await reconcile());
    } else if (msg.type === "save") {
      await chrome.storage.local.set(msg.settings);
      reply(await reconcile());
    } else if (msg.type === "refresh") {
      const s = await settings();
      try { await fetch(s.statusUrl + "/refresh", { method: "POST", headers: { "X-Sail": "1" } }); } catch (e) {}
      reply(await reconcile());
    } else if (msg.type === "rebuild") {
      const s = await settings();
      try { await fetch(s.statusUrl + "/rebuild", { method: "POST", headers: { "X-Sail": "1" } }); } catch (e) {}
      reply({ ok: true });
    }
  })();
  return true;
});
