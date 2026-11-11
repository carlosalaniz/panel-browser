# Panel Browser

Puts a web page on small touch screens, such as a desk clock or a wall dashboard, without a browser on the screen.

```
 Harbor machine                                         Raspberry Pi + touch screen
┌──────────────────────────────────┐   changed pixels   ┌───────────────────────────┐
│ app/  headless Chromium renders  │ ─────────────────► │ panel/  draws them into   │
│       the page, finds what       │    WebSocket       │         the framebuffer   │
│       changed, compresses it     │ ◄───────────────── │         and sends taps    │
└──────────────────────────────────┘        taps        └───────────────────────────┘
        announced on the LAN as _desk-panel._tcp, so the panel finds it by itself
```

A Raspberry Pi Zero W is too slow for a modern browser: pages take seconds to react. Here the page runs on a
real machine, and the Pi only copies the changed parts of the picture to its screen and sends taps back. Taps get
an instant ring on the Pi, before the page reacts. On a Pi Zero W, the panel holds 60 frames a second while up to
about a quarter of the screen changes. Full-screen motion runs at about 15 frames a second.

| Folder | What it is |
|---|---|
| [`app/`](app) | The renderer: Node + Playwright's Chromium, the panel stream and a settings page |
| [`harbor/`](harbor) | The [Harbor](https://github.com/carlosalaniz/harbor) package: add this repo as a git source and Harbor builds `app/` on the machine |
| [`panel/`](panel) | The panel client for the Pi, in Go: a static binary, no X, no browser |

## Run it on Harbor

Harbor 0.44.0 or newer, with LAN access on, so the app can announce itself:

```sh
harbor sources add https://github.com/carlosalaniz/panel-browser
```

Then install Panel Browser from the App Store, open it, and type the page to show. Panels on the network find it on
their own. See [`harbor/README.md`](harbor/README.md).

## Develop on a Mac

```sh
cd app && npm install
PANEL_URL=http://localhost:5173/ npm run dev   # panels on :5174, settings on http://localhost:5175
```

`npm run dev` also announces the renderer with macOS's `dns-sd`, the way Harbor does, so a panel on the same
network finds it.

## Protocol (version 1)

Over a WebSocket at `/panel`, all numbers little-endian:

| Direction | Message |
|---|---|
| panel → renderer | text `{"type":"hello","proto":1,"width":1024,"height":600,"name":"desk"}` |
| renderer → panel | text `{"type":"size","width":1024,"height":600}`: the page's size; the panel draws what fits |
| renderer → panel | binary: `x u16, y u16, w u16, h u16, length u32`, then zlib-compressed RGB565 pixels, row by row |
| panel → renderer | binary: `kind u8` (1 down, 2 move, 3 up), `finger u8, x u16, y u16` |

The renderer pings every 15 seconds. A panel that hears nothing for 45 seconds reconnects. A slow panel is never
queued behind old pictures: changes collect into one area until the panel has caught up, and then it gets the
newest pixels.

The announcement is DNS-SD type `_desk-panel._tcp` with TXT `path=/panel`, `proto=1` and `scheme=http`. Harbor
adds `app` and `harbor=1`.
