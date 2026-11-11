const $ = (id) => document.getElementById(id);
const form = $("url-form");
const input = $("url");
const problem = $("url-problem");
const statusLine = $("status");
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
$("reload").addEventListener("click", () => call("POST", "/api/reload", {}).then(refresh, () => {}));

// ---- live screen ----

const screen = $("screen");
const draw = screen.getContext("2d");
const liveLine = $("live");
const mac = /Mac|iPhone|iPad/.test(navigator.platform);
let socket = null;
let retry = null;

function send(message) {
  if (socket?.readyState === WebSocket.OPEN) socket.send(JSON.stringify(message));
}

// Draw the newest frame only: frames that arrive while one decodes replace each other.
let decoding = false;
let waiting = null;
async function paint(blob) {
  if (decoding) return void (waiting = blob);
  decoding = true;
  try {
    const image = await createImageBitmap(blob);
    if (screen.width !== image.width || screen.height !== image.height) {
      screen.width = image.width;
      screen.height = image.height;
    }
    draw.drawImage(image, 0, 0);
    image.close();
  } catch {}
  decoding = false;
  if (waiting) {
    const next = waiting;
    waiting = null;
    paint(next);
  }
}

function connect() {
  clearTimeout(retry);
  if (document.hidden || socket) return;
  const ws = new WebSocket(`${location.protocol === "https:" ? "wss" : "ws"}://${location.host}/api/live`);
  ws.binaryType = "blob";
  socket = ws;
  ws.onopen = () => (liveLine.textContent = "Live.");
  ws.onmessage = (event) => paint(event.data);
  ws.onclose = () => {
    if (socket !== ws) return;
    socket = null;
    liveLine.textContent = "Not connected. Trying again…";
    retry = setTimeout(connect, 2000);
  };
}
// A hidden tab does not need frames.
document.addEventListener("visibilitychange", () => {
  if (document.hidden) {
    const ws = socket;
    socket = null;
    ws?.close();
  } else connect();
});

function at(event) {
  const box = screen.getBoundingClientRect();
  return {
    x: Math.max(0, Math.min(screen.width - 1, ((event.clientX - box.left) / box.width) * screen.width)),
    y: Math.max(0, Math.min(screen.height - 1, ((event.clientY - box.top) / box.height) * screen.height)),
  };
}
const BUTTON = ["left", "middle", "right"];

// Moves go out at most once per frame.
let move = null;
function flushMove() {
  if (move) send({ type: "mouse", action: "move", ...move });
  move = null;
}
screen.addEventListener("pointerdown", (event) => {
  if (!event.isPrimary) return;
  event.preventDefault();
  screen.focus({ preventScroll: true });
  screen.setPointerCapture(event.pointerId);
  move = null;
  send({ type: "mouse", action: "down", button: BUTTON[event.button] ?? "left", ...at(event) });
});
screen.addEventListener("pointermove", (event) => {
  if (!event.isPrimary) return;
  if (!move) requestAnimationFrame(flushMove);
  move = at(event);
});
screen.addEventListener("pointerup", (event) => {
  if (!event.isPrimary) return;
  flushMove();
  send({ type: "mouse", action: "up", button: BUTTON[event.button] ?? "left", ...at(event) });
});
screen.addEventListener("contextmenu", (event) => event.preventDefault());
screen.addEventListener(
  "wheel",
  (event) => {
    event.preventDefault();
    const scale = event.deltaMode === 1 ? 40 : event.deltaMode === 2 ? screen.height : 1;
    send({ type: "wheel", ...at(event), dx: event.deltaX * scale, dy: event.deltaY * scale });
  },
  { passive: false },
);

// The keyboard, while the picture has focus. The page runs on Linux, so ⌘ becomes Ctrl on a Mac.
const keyName = (key) => (mac && key === "Meta" ? "Control" : key);
const pasting = (event) => (event.metaKey || event.ctrlKey) && event.key.toLowerCase() === "v";
const usable = (key) => !["Dead", "Process", "Unidentified"].includes(key);
screen.addEventListener("keydown", (event) => {
  if (pasting(event) || !usable(event.key)) return; // the paste event below sends the text
  event.preventDefault();
  send({ type: "key", action: "down", key: keyName(event.key) });
});
screen.addEventListener("keyup", (event) => {
  if (pasting(event) || !usable(event.key)) return;
  event.preventDefault();
  send({ type: "key", action: "up", key: keyName(event.key) });
});
screen.addEventListener("paste", (event) => {
  const text = event.clipboardData?.getData("text") ?? "";
  if (text) send({ type: "text", text });
  event.preventDefault();
});

// The text box: phones (their keyboard opens for a field, not a picture), accents and pasting anywhere.
// It keeps one invisible character, so a backspace on an "empty" box still reaches the page.
const typeBox = $("type");
const KEEP = "\u200b";
const press = (key) => {
  send({ type: "key", action: "down", key });
  send({ type: "key", action: "up", key });
};
const resetBox = () => {
  typeBox.value = KEEP;
  typeBox.setSelectionRange(1, 1);
};
typeBox.addEventListener("focus", resetBox);
typeBox.addEventListener("input", (event) => {
  if (event.isComposing) return;
  const value = typeBox.value;
  if (!value.includes(KEEP)) press("Backspace");
  const text = value.replaceAll(KEEP, "");
  if (text) send({ type: "text", text });
  resetBox();
});
typeBox.addEventListener("compositionend", () => typeBox.dispatchEvent(new Event("input")));
$("type-form").addEventListener("submit", (event) => {
  event.preventDefault();
  press("Enter");
});

refresh();
setInterval(refresh, 3000);
connect();
