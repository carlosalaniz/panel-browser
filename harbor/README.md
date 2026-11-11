# Panel Browser

Shows one web page on small touch screens, such as a Raspberry Pi with a 7-inch display on a desk or a wall.

The page runs here, in a real Chromium. A screen does not run a browser at all. It gets only the parts of the
picture that changed, and sends its taps back. A Raspberry Pi Zero, too slow for a modern browser, can show a
live dashboard this way.

## Set it up

1. Open Panel Browser. Type the address of the page to show and pick your time zone. The page's clock uses it.
2. Turn on a panel on the same network. It finds the app by itself, as long as Harbor's LAN access is on. On the
   panel, a three-finger swipe down opens its address screen, where you can choose another renderer or type an
   address.

The settings page shows the page live, and you can use it from there: click, drag, scroll and type, as on the
page itself. That is how you sign in to a page or fill in a pairing code without a keyboard on the panel. On a
phone, tap a field in the picture, then type in the box under it. ⌘ shortcuts work as Ctrl.

## Good to know

- **Discovery:** the app announces itself on the network as `_desk-panel._tcp`. The address points to the panel
  stream, an open WebSocket at `/panel`. Panels cannot do a browser login, so anyone on your network can watch the
  page and tap it. Only show pages you are fine with that for.
- **Other apps:** the browser runs in a container, so `.local` names do not resolve there. Use an IP address.
- **Screen size:** the page takes the size of the first panel that connects. Panels with a different size show
  the part that fits.
- **Fonts:** the browser has DejaVu, Noto and Noto Color Emoji. A page that brings its own web font looks
  the same everywhere.
