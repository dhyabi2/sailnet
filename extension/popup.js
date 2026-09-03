// setState renders "text [pill]" without innerHTML (store reviewers reject it).
function setState(text, pill, cls) {
  const el = document.getElementById("state");
  el.textContent = text + " ";
  const span = document.createElement("span");
  span.className = "pill " + cls;
  span.textContent = pill;
  el.appendChild(span);
}
const $ = id => document.getElementById(id);
const send = msg => new Promise(r => chrome.runtime.sendMessage(msg, r));

function human(b) {
  if (b > 1e9) return (b / 1e9).toFixed(2) + " GB";
  if (b > 1e6) return (b / 1e6).toFixed(1) + " MB";
  if (b > 1e3) return (b / 1e3).toFixed(0) + " kB";
  return (b || 0) + " B";
}

async function render() {
  const { settings, status } = await send({ type: "status" });
  const on = settings.enabled;
  $("toggle").textContent = on ? "Turn off" : "Turn on";
  if (!on) {
    setState("Off", "direct", "warn");
    $("hint").textContent = "Pages load directly. Turn on to route Chrome through Sailnet.";
  } else if (status && status.path) {
    setState("On", status.hops + " hops", "on");
    $("hint").textContent = "";
  } else if (status) {
    setState("Building circuit", "wait", "warn");
    $("hint").textContent = "Paying the entry relay and extending the circuit. Pages will load in a few seconds.";
  } else {
    setState("Client not running", "blocked", "warn");
    $("hint").textContent = "The proxy is fixed to the client's port, so nothing leaks: pages will not load until you start `sailnode client`.";
  }
  if (status) {
    $("path").textContent = status.path || "—";
    $("balance").textContent = status.balance ? status.balance + " XNO" : "unknown";
    $("traffic").textContent = "↑ " + human(status.bytesUp) + "  ↓ " + human(status.bytesDown) + "  " + status.relays + " relays";
    $("address").textContent = status.address || "—";
    if (status.nick) document.querySelector("h1").textContent = "Sailnet · " + status.nick;
  }
  $("socksHost").value = settings.socksHost;
  $("socksPort").value = settings.socksPort;
  $("statusUrl").value = settings.statusUrl;
}

$("toggle").onclick = async () => {
  const { settings } = await send({ type: "status" });
  await send({ type: "toggle", enabled: !settings.enabled });
  render();
};
$("rebuild").onclick = async () => { await send({ type: "rebuild" }); setTimeout(render, 1500); };
$("copy").onclick = () => navigator.clipboard.writeText($("address").textContent);
$("save").onclick = async () => {
  await send({ type: "save", settings: { socksHost: $("socksHost").value.trim(), socksPort: Number($("socksPort").value), statusUrl: $("statusUrl").value.trim().replace(/\/$/, "") } });
  render();
};

render();
setInterval(render, 2000);
