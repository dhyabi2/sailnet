// Sailnet browser extension: turns Chrome's proxy on and off and keeps the
// toolbar badge in sync with the local client's status endpoint.

const DEFAULTS = { socksHost: "127.0.0.1", socksPort: 1080, statusUrl: "http://127.0.0.1:1090", enabled: false, killSwitch: true };

async function settings() {
  return { ...DEFAULTS, ...(await chrome.storage.local.get(Object.keys(DEFAULTS))) };
}

// Chrome resolves hostnames at the SOCKS5 proxy itself for scheme "socks5",
// so no DNS query leaves the machine for proxied traffic.
async function applyProxy(on) {
  const s = await settings();
  if (on) {
    await chrome.proxy.settings.set({
      value: {
        mode: "fixed_servers",
        rules: {
          singleProxy: { scheme: "socks5", host: s.socksHost, port: Number(s.socksPort) },
          bypassList: ["127.0.0.1", "localhost", "<-loopback>"]
        }
      },
      scope: "regular"
    });
    // WebRTC must not reveal the real address around the proxy.
    try { await chrome.privacy.network.webRTCIPHandlingPolicy.set({ value: "disable_non_proxied_udp" }); } catch (e) {}
  } else {
    await chrome.proxy.settings.clear({ scope: "regular" });
    try { await chrome.privacy.network.webRTCIPHandlingPolicy.clear({}); } catch (e) {}
  }
}

async function status() {
  const s = await settings();
  try {
    const r = await fetch(s.statusUrl + "/status", { cache: "no-store", headers: { "X-Sail": "1" } });
    if (!r.ok) throw new Error("http " + r.status);
    return await r.json();
  } catch (e) {
    return null;
  }
}

async function refreshBadge() {
  const s = await settings();
  const st = await status();
  if (!s.enabled) {
    await chrome.action.setBadgeText({ text: "" });
    await chrome.action.setTitle({ title: "Sailnet: off" });
    return;
  }
  if (st && st.path) {
    await chrome.action.setBadgeBackgroundColor({ color: "#0E6F6A" });
    await chrome.action.setBadgeText({ text: "ON" });
    await chrome.action.setTitle({ title: "Sailnet: " + st.path });
  } else {
    await chrome.action.setBadgeBackgroundColor({ color: "#B3731A" });
    await chrome.action.setBadgeText({ text: st ? "…" : "!" });
    await chrome.action.setTitle({ title: st ? "Sailnet: building circuit" : "Sailnet: client not running (sailnode client)" });
    // Kill switch: with the proxy fixed to a dead SOCKS port, Chrome simply
    // fails to load pages rather than leaking around it. Nothing to do.
  }
}

chrome.runtime.onInstalled.addListener(async () => {
  const s = await settings();
  await applyProxy(s.enabled);
  await chrome.alarms.create("tick", { periodInMinutes: 0.25 });
  refreshBadge();
});
chrome.runtime.onStartup.addListener(async () => {
  const s = await settings();
  await applyProxy(s.enabled);
  await chrome.alarms.create("tick", { periodInMinutes: 0.25 });
  refreshBadge();
});
chrome.alarms.onAlarm.addListener(a => { if (a.name === "tick") refreshBadge(); });

chrome.runtime.onMessage.addListener((msg, _sender, reply) => {
  (async () => {
    if (msg.type === "toggle") {
      await chrome.storage.local.set({ enabled: msg.enabled });
      await applyProxy(msg.enabled);
      await refreshBadge();
      reply({ ok: true });
    } else if (msg.type === "status") {
      reply({ settings: await settings(), status: await status() });
    } else if (msg.type === "save") {
      await chrome.storage.local.set(msg.settings);
      const s = await settings();
      await applyProxy(s.enabled);
      await refreshBadge();
      reply({ ok: true });
    } else if (msg.type === "rebuild") {
      const s = await settings();
      try { await fetch(s.statusUrl + "/rebuild", { method: "POST", headers: { "X-Sail": "1" } }); } catch (e) {}
      reply({ ok: true });
    }
  })();
  return true;
});
