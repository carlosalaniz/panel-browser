# Panel client (Raspberry Pi)

A static Go binary that shows a Panel Browser renderer on the Pi's screen:

- It draws straight into `/dev/fb0`, a 16-bit RGB565 framebuffer, and reads multitouch from evdev.
- It needs no X, no browser and no avahi.
- It uses about 2 MB of memory.

## What it does

- **Finds renderers by itself:** it asks the network for `_desk-panel._tcp` (mDNS) and takes the only one it finds.
  It remembers that renderer by name and looks the name up again on every connect, so it follows the renderer to a
  new address.
- **Three-finger swipe down:** opens the address screen. It lists the renderers found on the network, and its
  keyboard lets you type an address instead (`host:port`, or a pasted `http://` URL).
- **Taps:** a ring shows where your finger landed at once. A small dot in the bottom-right corner means the
  renderer can't be reached, and the client keeps trying.
- **`desk-panel -find`:** lists the renderers this Pi can see.

## Build and install

```sh
cd panel
GOOS=linux GOARCH=arm GOARM=6 CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o desk-panel .
```

`GOARM=6` covers the Pi Zero and Pi 1. A Pi Zero 2, 3 or 4 running a 32-bit OS also runs it. On a 64-bit OS, use
`GOARCH=arm64`.

On the Pi, as root:

```sh
install -D -m 755 desk-panel /opt/desk-panel/desk-panel
install -m 644 desk-panel.service /etc/systemd/system/
systemctl daemon-reload && systemctl enable --now desk-panel
```

The choice is kept in `/etc/default/desk-panel`: `PANEL_FIND="<renderer name>"`, or `PANEL_ADDR="ws://host:port/panel"`
for a typed address. Delete the file to go back to finding the renderer on its own.

## The screen

- **Framebuffer:** it must be 16 bits per pixel. The legacy (non-KMS) driver gives that by default. Set the
  resolution in `config.txt`, for example `hdmi_cvt=1024 600 60 6 0 0 0` for a 7-inch 1024×600 screen.
- **USB touch:** plug the touch cable straight into the Pi. Some USB hubs never pass the touch device through.
