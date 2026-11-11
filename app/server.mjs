// Panel Browser: keeps one web page open in headless Chromium and streams only the changed screen
// area to small display clients (a Raspberry Pi touch panel). Taps come back as mouse input.
//
// Two HTTP listeners:
//   STREAM_PORT (8080)    GET /healthz, WebSocket /panel  (open: panels on the LAN cannot do a browser login)
//   SETTINGS_PORT (8081)  the settings page, its JSON API and WebSocket /api/live (Harbor puts its app login in front)
//
// Live screen (/api/live, the settings page): the renderer sends each repaint as a PNG (binary); the page sends
// input as JSON text: {"type":"mouse","action":"down"|"move"|"up","x","y","button":"left"|"right"|"middle"},
// {"type":"wheel","x","y","dx","dy"}, {"type":"key","action":"down"|"up","key":"<KeyboardEvent.key>"},
// {"type":"text","text":"..."}.
//
// Panel protocol, version 1 (WebSocket, little-endian):
//   panel -> renderer  text   {"type":"hello","proto":1,"width":1024,"height":600,"name":"desk"}
//   renderer -> panel  text   {"type":"size","width":1024,"height":600}  (the page's size; draw what fits)
//   renderer -> panel  binary x u16, y u16, w u16, h u16, length u32, then zlib(RGB565 pixels, row by row)
//   panel -> renderer  binary kind u8 (1 down, 2 move, 3 up), finger u8, x u16, y u16
import { spawn } from "node:child_process";
import { readFileSync, writeFileSync, renameSync, mkdirSync } from "node:fs";
import { createServer } from "node:http";
import { deflateSync } from "node:zlib";
import { chromium } from "playwright-core";
import { PNG } from "pngjs";
import { WebSocketServer } from "ws";

const here = new URL(".", import.meta.url).pathname;
const dataDir = process.env.DATA_DIR ?? `${here}data`;
const streamPort = Number(process.env.STREAM_PORT ?? 8080);
const settingsPort = Number(process.env.SETTINGS_PORT ?? 8081);
const configFile = `${dataDir}/config.json`;
const DEFAULT_SIZE = { width: 1024, height: 600 };
const MAX_SIZE = 4096;

// ---- settings ----

function loadConfig() {
  try {
    const c = JSON.parse(readFileSync(configFile, "utf8"));
    if (typeof c.url === "string") return { url: c.url, timezone: checkZone(c.timezone) ? c.timezone : "" };
  } catch {}
  return { url: process.env.PANEL_URL ?? "", timezone: checkZone(process.env.TZ) ? process.env.TZ : "" };
}
function saveConfig(c) {
  mkdirSync(dataDir, { recursive: true });
  writeFileSync(`${configFile}.tmp`, JSON.stringify(c, null, 2) + "\n");
  renameSync(`${configFile}.tmp`, configFile);
}
function checkUrl(text) {
  let u;
  try {
    u = new URL(String(text).trim());
  } catch {
    return [null, "That is not a web address. Start it with http:// or https://."];
  }
  if (u.protocol !== "http:" && u.protocol !== "https:") return [null, "Only http:// and https:// addresses can be shown."];
  return [u.href, null];
}
function checkZone(zone) {
  return typeof zone === "string" && (zone === "UTC" || Intl.supportedValuesOf("timeZone").includes(zone));
}
let config = loadConfig();

// ---- pixels ----

let size = { ...DEFAULT_SIZE };
let frame = null; // the page as RGB565, size.width * size.height
const clients = new Set(); // { ws, dirty, inFlight, who, name, width, height, since }

function toRgb565(png) {
  const { width: W, height: H } = size;
  const out = new Uint16Array(W * H);
  const src = png.data;
  const w = Math.min(png.width, W);
  const h = Math.min(png.height, H);
  for (let y = 0; y < h; y++) {
    for (let x = 0; x < w; x++) {
      const i = (y * png.width + x) * 4;
      out[y * W + x] = ((src[i] >> 3) << 11) | ((src[i + 1] >> 2) << 5) | (src[i + 2] >> 3);
    }
  }
  return out;
}

// Smallest rectangle that holds every changed pixel, or null when nothing changed.
function changedArea(prev, next) {
  const { width: W, height: H } = size;
  let minX = W, minY = H, maxX = -1, maxY = -1;
  for (let y = 0; y < H; y++) {
    const row = y * W;
    for (let x = 0; x < W; x++) {
      if (prev[row + x] !== next[row + x]) {
        if (x < minX) minX = x;
        if (x > maxX) maxX = x;
        if (y < minY) minY = y;
        maxY = y;
      }
    }
  }
  return maxX < 0 ? null : { x: minX, y: minY, w: maxX - minX + 1, h: maxY - minY + 1 };
}

function encode(pixels, { x, y, w, h }) {
  const W = size.width;
  const raw = Buffer.alloc(w * h * 2);
  const all = Buffer.from(pixels.buffer, pixels.byteOffset, pixels.byteLength);
  for (let row = 0; row < h; row++) {
    all.copy(raw, row * w * 2, ((y + row) * W + x) * 2, ((y + row) * W + x + w) * 2);
  }
  const body = deflateSync(raw, { level: 3 });
  const head = Buffer.alloc(12);
  head.writeUInt16LE(x, 0);
  head.writeUInt16LE(y, 2);
  head.writeUInt16LE(w, 4);
  head.writeUInt16LE(h, 6);
  head.writeUInt32LE(body.length, 8);
  return Buffer.concat([head, body]);
}

function union(a, b) {
  if (!a) return b;
  const x = Math.min(a.x, b.x);
  const y = Math.min(a.y, b.y);
  return { x, y, w: Math.max(a.x + a.w, b.x + b.w) - x, h: Math.max(a.y + a.h, b.y + b.h) - y };
}

// Backpressure: while a panel is still receiving earlier updates, changes pile up in its dirty
// area instead of a queue, so a slow panel gets the newest pixels instead of falling behind.
const MAX_IN_FLIGHT = 64 * 1024;
const stats = { captured: 0, changed: 0, sent: 0, bytes: 0, workMs: 0 };

function flush(client) {
  if (!client.dirty || !frame || client.inFlight > MAX_IN_FLIGHT || client.ws.readyState !== 1) return;
  const msg = encode(frame, client.dirty);
  client.dirty = null;
  client.inFlight += msg.length;
  stats.sent++;
  stats.bytes += msg.length;
  client.ws.send(msg, { binary: true }, () => {
    client.inFlight -= msg.length;
    flush(client);
  });
}

// Live screens on the settings page get the screencast's PNG as it is, newest only: while one is still
// sending, a newer frame replaces the waiting one.
const viewers = new Set(); // { ws, sending, next }
let lastPng = null;
function sendLive(viewer, png) {
  if (viewer.ws.readyState !== 1) return;
  if (viewer.sending) {
    viewer.next = png;
    return;
  }
  viewer.sending = true;
  viewer.ws.send(png, { binary: true }, () => {
    viewer.sending = false;
    const next = viewer.next;
    viewer.next = null;
    if (next) sendLive(viewer, next);
  });
}

function show(png) {
  const next = toRgb565(png);
  const area = frame ? changedArea(frame, next) : { x: 0, y: 0, w: size.width, h: size.height };
  frame = next;
  if (!area) return;
  stats.changed++;
  for (const c of clients) {
    c.dirty = union(c.dirty, area);
    flush(c);
  }
}

setInterval(() => {
  if (!stats.captured) return;
  console.log(
    `renderer: ${(stats.captured / 10).toFixed(1)} captures/s, ${(stats.changed / 10).toFixed(1)} changed/s, ` +
      `${(stats.sent / 10).toFixed(1)} sent/s, ${(stats.bytes / 10 / 1024).toFixed(0)} KB/s, ` +
      `${(stats.workMs / stats.captured).toFixed(1)} ms per capture`,
  );
  Object.keys(stats).forEach((k) => (stats[k] = 0));
}, 10000);

// Process only the newest frame; the screencast delivers bursts while the page animates.
let pending = null;
let busy = false;
function queue(base64) {
  pending = base64;
  if (busy) return;
  busy = true;
  setImmediate(function work() {
    const data = pending;
    pending = null;
    const started = performance.now();
    try {
      const png = Buffer.from(data, "base64");
      lastPng = png;
      for (const v of viewers) sendLive(v, png);
      show(PNG.sync.read(png));
    } catch (err) {
      console.error("frame failed:", err.message);
    }
    stats.captured++;
    stats.workMs += performance.now() - started;
    if (pending) setImmediate(work);
    else busy = false;
  });
}

// ---- the browser ----

const browser = await chromium.launch({ args: ["--disable-dev-shm-usage"] });
const page = await browser.newPage({ viewport: size, deviceScaleFactor: 1 });
const cdp = await page.context().newCDPSession(page);
const pageState = { status: "idle", error: null, title: "" };

// Screencast sends a frame whenever the page repaints, so an idle page costs nothing.
cdp.on("Page.screencastFrame", ({ data, sessionId }) => {
  cdp.send("Page.screencastFrameAck", { sessionId }).catch(() => {});
  queue(data);
});
async function startScreencast() {
  await cdp.send("Page.stopScreencast").catch(() => {});
  await cdp.send("Page.startScreencast", { format: "png", maxWidth: size.width, maxHeight: size.height, everyNthFrame: 1 });
}
await startScreencast();

// The page's clock follows the chosen time zone (a container runs in UTC); none = the system's.
async function applyZone() {
  await cdp.send("Emulation.setTimezoneOverride", { timezoneId: config.timezone }).catch((err) =>
    console.error("time zone:", err.message),
  );
}
await applyZone();

let generation = 0;
async function navigate(url) {
  const mine = ++generation;
  if (!url) {
    Object.assign(pageState, { status: "idle", error: null, title: "" });
    await page.setContent(blankPage()).catch(() => {});
    return;
  }
  for (let attempt = 0; mine === generation; attempt++) {
    Object.assign(pageState, { status: "loading", error: attempt ? pageState.error : null });
    try {
      await page.goto(url, { waitUntil: "load", timeout: 20000 });
      if (mine !== generation) return;
      Object.assign(pageState, { status: "loaded", error: null, title: await page.title().catch(() => "") });
      console.log(`showing ${url}`);
      return;
    } catch (err) {
      if (mine !== generation) return;
      const why = err.message.split("\n")[0];
      Object.assign(pageState, { status: "error", error: why });
      console.warn(`cannot open ${url} (${why}), trying again in 5 s`);
      await new Promise((r) => setTimeout(r, 5000));
    }
  }
}
page.on("crash", () => {
  console.error("the page crashed, opening it again");
  navigate(config.url);
});

function blankPage() {
  return `<!doctype html><meta charset="utf-8"><body style="margin:0;height:100vh;display:grid;place-items:center;
    background:#101418;color:#9fb3c8;font:24px system-ui,sans-serif"><div style="text-align:center">
    <div style="color:#f2f4f7;font-size:32px;margin-bottom:12px">Nothing to show yet</div>
    Open Panel Browser in Harbor and choose a page.</div>`;
}

async function resize(width, height) {
  if (width === size.width && height === size.height) return;
  console.log(`page size ${width}x${height}`);
  size = { width, height };
  frame = null;
  await page.setViewportSize(size);
  await startScreencast();
}

// Input from panels and live screens runs one step at a time, in order.
let input = Promise.resolve();
function act(step) {
  input = input.then(step).catch((err) => console.error("input failed:", err.message.split("\n")[0]));
}
// A burst of moves becomes one: a waiting move takes the newest position.
let nextMove = null;
function moveTo(x, y) {
  if (nextMove) return void Object.assign(nextMove, { x, y });
  nextMove = { x, y };
  act(async () => {
    const { x: mx, y: my } = nextMove;
    nextMove = null;
    await page.mouse.move(mx, my);
  });
}

// Taps: the first finger drives the mouse.
function touch(kind, finger, x, y) {
  if (finger !== 0) return;
  if (kind === 2) return moveTo(x, y);
  act(async () => {
    await page.mouse.move(x, y);
    if (kind === 1) await page.mouse.down();
    else if (kind === 3) await page.mouse.up();
  });
}

// ---- stream listener (panels) ----

const streamServer = createServer((req, res) => {
  if (req.url === "/healthz") {
    res.writeHead(200, { "content-type": "text/plain" }).end("ok\n");
    return;
  }
  res.writeHead(404, { "content-type": "text/plain" }).end("Panels connect to /panel with a WebSocket.\n");
});
const wss = new WebSocketServer({ noServer: true, perMessageDeflate: false, maxPayload: 64 * 1024 });
streamServer.on("upgrade", (req, socket, head) => {
  if (new URL(req.url, "http://x").pathname !== "/panel") {
    socket.end("HTTP/1.1 404 Not Found\r\n\r\n");
    return;
  }
  wss.handleUpgrade(req, socket, head, (ws) => connected(ws, req));
});

function connected(ws, req) {
  const who = String(req.headers["x-forwarded-for"] ?? req.socket.remoteAddress).replace(/^::ffff:/, "");
  const client = { ws, dirty: null, inFlight: 0, who, name: "", width: 0, height: 0, since: new Date().toISOString(), ready: false };
  ws._socket?.setNoDelay(true);
  console.log(`panel connected: ${who}`);
  const alive = setInterval(() => ws.ping(), 15000); // lets the panel notice a dead link
  ws.on("message", async (data, isBinary) => {
    if (isBinary) {
      for (let i = 0; i + 6 <= data.length; i += 6) touch(data[i], data[i + 1], data.readUInt16LE(i + 2), data.readUInt16LE(i + 4));
      return;
    }
    let msg;
    try {
      msg = JSON.parse(data.toString());
    } catch {
      return;
    }
    if (msg.type !== "hello" || client.ready) return;
    client.name = String(msg.name ?? "").slice(0, 60);
    client.width = clamp(msg.width);
    client.height = clamp(msg.height);
    // The page follows the first panel that connects; later panels with another size get what fits.
    const others = [...clients].filter((c) => c.ready);
    if (others.length === 0 && client.width && client.height) await resize(client.width, client.height);
    client.ready = true;
    clients.add(client);
    ws.send(JSON.stringify({ type: "size", ...size }));
    client.dirty = { x: 0, y: 0, w: size.width, h: size.height };
    flush(client);
  });
  const drop = () => {
    clearInterval(alive);
    if (clients.delete(client)) console.log(`panel disconnected: ${who}`);
  };
  ws.on("close", drop);
  ws.on("error", drop);
}
const clamp = (v) => (Number.isInteger(v) && v > 0 && v <= MAX_SIZE ? v : 0);

// ---- settings listener ----

const files = {
  "/": ["settings.html", "text/html; charset=utf-8"],
  "/settings.js": ["settings.js", "text/javascript; charset=utf-8"],
  "/settings.css": ["settings.css", "text/css; charset=utf-8"],
};
const security = {
  "content-security-policy": "default-src 'self'; img-src 'self' blob:; style-src 'self'; script-src 'self'; frame-ancestors 'none'",
  "x-content-type-options": "nosniff",
  "cache-control": "no-store",
};

function json(res, status, body) {
  res.writeHead(status, { ...security, "content-type": "application/json" }).end(JSON.stringify(body));
}
async function readJson(req) {
  // A JSON content type keeps other sites from posting here (it needs a CORS preflight).
  if (!String(req.headers["content-type"] ?? "").startsWith("application/json")) return null;
  let body = "";
  for await (const chunk of req) {
    body += chunk;
    if (body.length > 16384) return null;
  }
  try {
    return JSON.parse(body || "{}");
  } catch {
    return null;
  }
}
function previewPng() {
  const { width: W, height: H } = size;
  const png = new PNG({ width: W, height: H });
  for (let i = 0; i < W * H; i++) {
    const v = frame ? frame[i] : 0;
    const r = v >> 11, g = (v >> 5) & 63, b = v & 31;
    png.data[i * 4] = (r << 3) | (r >> 2);
    png.data[i * 4 + 1] = (g << 2) | (g >> 4);
    png.data[i * 4 + 2] = (b << 3) | (b >> 2);
    png.data[i * 4 + 3] = 255;
  }
  return PNG.sync.write(png, { deflateLevel: 3 });
}

const settingsServer = createServer(async (req, res) => {
  const path = new URL(req.url, "http://x").pathname;
  try {
    if (req.method === "GET" && path === "/healthz") return res.writeHead(200).end("ok\n");
    if (req.method === "GET" && files[path]) {
      const [file, type] = files[path];
      return res.writeHead(200, { ...security, "content-type": type }).end(readFileSync(`${here}web/${file}`));
    }
    if (req.method === "GET" && path === "/api/state") {
      return json(res, 200, {
        url: config.url,
        timezone: config.timezone,
        page: { ...pageState, width: size.width, height: size.height },
        panels: [...clients].map(({ who, name, width, height, since }) => ({ address: who, name, width, height, since })),
      });
    }
    if (req.method === "GET" && path === "/api/preview.png") {
      return res.writeHead(200, { ...security, "content-type": "image/png" }).end(previewPng());
    }
    if (req.method === "PUT" && path === "/api/settings") {
      const body = await readJson(req);
      if (!body) return json(res, 400, { error: "Send JSON." });
      const empty = String(body.url ?? "").trim() === "";
      const [url, problem] = empty ? ["", null] : checkUrl(body.url);
      if (problem) return json(res, 400, { error: problem });
      const timezone = body.timezone ?? config.timezone;
      if (timezone !== "" && !checkZone(timezone)) return json(res, 400, { error: "Pick a time zone from the list." });
      const zoneChanged = timezone !== config.timezone;
      const urlChanged = url !== config.url;
      config = { ...config, url, timezone };
      saveConfig(config);
      if (zoneChanged) await applyZone();
      // A new time zone reaches scripts that already read the time only after a reload.
      if (urlChanged || zoneChanged) navigate(url);
      return json(res, 200, { url, timezone });
    }
    if (req.method === "POST" && path === "/api/reload") {
      if (!(await readJson(req))) return json(res, 400, { error: "Send JSON." });
      navigate(config.url);
      return json(res, 200, {});
    }
    json(res, 404, { error: "Not found." });
  } catch (err) {
    console.error("settings request failed:", err);
    if (!res.headersSent) json(res, 500, { error: "Something went wrong." });
  }
});

// ---- live screen (settings page) ----

// The settings page's own address only: another site open in the same browser must not drive the page
// with the visitor's Harbor login. Behind Harbor's proxies the address the browser used is X-Forwarded-Host.
function sameSite(req) {
  let origin;
  try {
    origin = new URL(req.headers.origin);
  } catch {
    return false;
  }
  const host = String(req.headers["x-forwarded-host"] ?? req.headers.host ?? "").split(",")[0].trim().toLowerCase();
  return origin.host.toLowerCase() === host;
}

const live = new WebSocketServer({ noServer: true, perMessageDeflate: false, maxPayload: 16 * 1024 });
settingsServer.on("upgrade", (req, socket, head) => {
  if (new URL(req.url, "http://x").pathname !== "/api/live") return void socket.end("HTTP/1.1 404 Not Found\r\n\r\n");
  if (!sameSite(req)) return void socket.end("HTTP/1.1 403 Forbidden\r\n\r\n");
  live.handleUpgrade(req, socket, head, watching);
});

const BUTTONS = new Set(["left", "right", "middle"]);
const inPage = (x, y) => Number.isFinite(x) && Number.isFinite(y) && x >= 0 && y >= 0 && x < size.width && y < size.height;

function watching(ws) {
  const viewer = { ws, sending: false, next: null, keys: new Set(), buttons: new Set() };
  viewers.add(viewer);
  ws._socket?.setNoDelay(true);
  if (lastPng) sendLive(viewer, lastPng);
  const alive = setInterval(() => ws.ping(), 30000);
  ws.on("message", (data, isBinary) => {
    if (isBinary) return;
    let m;
    try {
      m = JSON.parse(data.toString());
    } catch {
      return;
    }
    const x = Math.round(Number(m.x)), y = Math.round(Number(m.y));
    if (m.type === "mouse" && inPage(x, y)) {
      if (m.action === "move") return moveTo(x, y);
      const button = BUTTONS.has(m.button) ? m.button : "left";
      if (m.action === "down") viewer.buttons.add(button);
      else if (m.action === "up") viewer.buttons.delete(button);
      else return;
      act(async () => {
        await page.mouse.move(x, y);
        await (m.action === "down" ? page.mouse.down({ button }) : page.mouse.up({ button }));
      });
    } else if (m.type === "wheel" && inPage(x, y)) {
      const dx = Math.max(-2000, Math.min(2000, Number(m.dx) || 0));
      const dy = Math.max(-2000, Math.min(2000, Number(m.dy) || 0));
      act(async () => {
        await page.mouse.move(x, y);
        await page.mouse.wheel(dx, dy);
      });
    } else if (m.type === "key" && typeof m.key === "string" && m.key.length > 0 && m.key.length <= 32) {
      if (m.action === "down") {
        viewer.keys.add(m.key);
        act(() => page.keyboard.down(m.key));
      } else if (m.action === "up") {
        viewer.keys.delete(m.key);
        act(() => page.keyboard.up(m.key));
      }
    } else if (m.type === "text" && typeof m.text === "string" && m.text.length > 0 && m.text.length <= 4000) {
      act(() => page.keyboard.insertText(m.text));
    }
  });
  const drop = () => {
    clearInterval(alive);
    if (!viewers.delete(viewer)) return;
    // a closed tab must not leave Shift or a mouse button held on the page
    for (const key of viewer.keys) act(() => page.keyboard.up(key));
    for (const button of viewer.buttons) act(() => page.mouse.up({ button }));
  };
  ws.on("close", drop);
  ws.on("error", drop);
}

streamServer.listen(streamPort, () => console.log(`panels: ws://*:${streamPort}/panel`));
settingsServer.listen(settingsPort, () => console.log(`settings: http://*:${settingsPort}/`));
navigate(config.url);

// On a developer machine, announce the stream like Harbor does (macOS dns-sd).
if (process.env.PANEL_ANNOUNCE) {
  const name = process.env.PANEL_ANNOUNCE;
  // A small shell keeps dns-sd only while this process lives: our end of its stdin closes even when we are
  // killed with SIGKILL, `read` returns, and the shell stops dns-sd (otherwise it would announce forever).
  const args = ["-R", name, "_desk-panel._tcp", "local", String(streamPort), "scheme=http", "path=/panel", "proto=1"];
  spawn("sh", ["-c", 'dns-sd "$@" >/dev/null & c=$!; read _; kill $c', "sh", ...args], { stdio: ["pipe", "ignore", "ignore"] });
  console.log(`announcing "${name}" as _desk-panel._tcp on port ${streamPort}`);
}
for (const sig of ["SIGINT", "SIGTERM"]) {
  process.on(sig, () => {
    browser.close().finally(() => process.exit(0));
    setTimeout(() => process.exit(0), 2000).unref();
  });
}
