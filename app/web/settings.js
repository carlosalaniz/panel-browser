const $ = (id) => document.getElementById(id);
const form = $("url-form");
const input = $("url");
const problem = $("url-problem");
const statusLine = $("status");
const preview = $("preview");
const zone = $("timezone");
const browserZone = Intl.DateTimeFormat().resolvedOptions().timeZone;
const zones = ["UTC", ...Intl.supportedValuesOf("timeZone").filter((z) => z !== "UTC")];
zone.replaceChildren(...zones.map((z) => new Option(z === browserZone ? `${z} (this browser)` : z, z)));
let size = { width: 1024, height: 600 };
let typed = false;

async function call(method, path, body) {
  const res = await fetch(path, {
    method,
    headers: body === undefined ? {} : { "content-type": "application/json" },
    body: body === undefined ? undefined : JSON.stringify(body),
  });
  const data = await res.json().catch(() => ({}));
  if (!res.ok) throw new Error(data.error ?? `The app answered ${res.status}.`);
  return data;
}

function describe(page, url) {
  if (!url) return "No page chosen yet. Type an address above.";
  if (page.status === "loading") return `Opening ${url}…`;
  if (page.status === "error") return `Cannot open ${url}: ${page.error}. Trying again.`;
  return `Showing ${page.title || url} at ${page.width} × ${page.height}.`;
}

function since(iso) {
  return new Date(iso).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" });
}

async function refresh() {
  try {
    const state = await call("GET", "/api/state");
    if (!typed) {
      input.value = state.url;
      zone.value = state.timezone || browserZone; // nothing saved yet: offer this browser's zone
    }
    size = { width: state.page.width, height: state.page.height };
    statusLine.textContent = describe(state.page, state.url);
    const list = $("panels");
    list.replaceChildren(
      ...state.panels.map((p) => {
        const li = document.createElement("li");
        const name = document.createElement("div");
        name.textContent = p.name || "Panel";
        const detail = document.createElement("p");
        detail.className = "quiet";
        detail.textContent = `${p.address} · ${p.width} × ${p.height} · connected at ${since(p.since)}`;
        li.append(name, detail);
        return li;
      }),
    );
    $("no-panels").hidden = state.panels.length > 0;
  } catch (err) {
    statusLine.textContent = err.message;
  }
}

let lastUrl = null;
async function refreshPreview() {
  if (document.hidden) return;
  try {
    const res = await fetch("/api/preview.png");
    if (!res.ok) return;
    const url = URL.createObjectURL(await res.blob());
    preview.src = url;
    if (lastUrl) URL.revokeObjectURL(lastUrl);
    lastUrl = url;
  } catch {}
}

input.addEventListener("input", () => (typed = true));
zone.addEventListener("change", () => (typed = true));
form.addEventListener("submit", async (event) => {
  event.preventDefault();
  problem.hidden = true;
  try {
    await call("PUT", "/api/settings", { url: input.value, timezone: zone.value });
    typed = false;
    refresh();
  } catch (err) {
    problem.textContent = err.message;
    problem.hidden = false;
  }
});
$("reload").addEventListener("click", () => call("POST", "/api/reload", {}).then(refresh, () => {}));
$("preview-button").addEventListener("click", (event) => {
  if (event.detail === 0) return; // keyboard activation has no position
  const box = preview.getBoundingClientRect();
  const x = ((event.clientX - box.left) / box.width) * size.width;
  const y = ((event.clientY - box.top) / box.height) * size.height;
  call("POST", "/api/tap", { x, y }).then(() => setTimeout(refreshPreview, 250), () => {});
});

refresh();
refreshPreview();
setInterval(refresh, 3000);
setInterval(refreshPreview, 1000);
