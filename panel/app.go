// Which renderer to show, the connection to it, and routing touches.
//
// The choice lives in PANEL_CONFIG (default /etc/default/desk-panel, read by systemd as an
// EnvironmentFile): PANEL_FIND = the name of a renderer found on the LAN, looked up again on every
// connect so it follows the renderer to a new address; PANEL_ADDR = an address typed on the panel.
// With neither, the panel looks around and takes the only renderer it finds.
package main

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/gorilla/websocket"
)

const defaultPort = "5174"

var errSwitch = errors.New("renderer changed")

type choice struct {
	addr string // a typed address as a ws:// URL
	find string // a renderer's announced name
}

type app struct {
	s          *screen
	configPath string
	send       chan touch    // touches for the current renderer session
	reconnect  chan struct{} // ends the current session after the choice changes
	fonts      *fonts
	art        chan *art // holds the pre-drawn address screen once it is ready

	mu     sync.Mutex
	choice choice
	status string
	found  []renderer
	ui     *overlay // the address screen, nil while closed
}

func (a *app) current() choice {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.choice
}

// address is the typed address in its short form, for the address field ("" when none).
func (a *app) address() string {
	return shortAddr(a.current().addr)
}

func (a *app) setStatus(text string) {
	a.mu.Lock()
	a.status = text
	ui := a.ui
	a.mu.Unlock()
	if ui != nil {
		ui.showStatus(text)
	}
}

func (a *app) setFound(found []renderer) {
	a.mu.Lock()
	a.found = found
	ui := a.ui
	a.mu.Unlock()
	if ui != nil {
		ui.showFound(found)
	}
}

// ---- addresses ----

// normalizeAddr accepts "host", "host:port", "host:port/path" or a pasted http(s)/ws(s) URL and
// returns the stream's WebSocket URL, or a problem to show.
func normalizeAddr(text string) (string, string) {
	t := strings.TrimSpace(text)
	if t == "" {
		return "", "Type an address first"
	}
	if !strings.Contains(t, "://") {
		t = "ws://" + t
	}
	u, err := url.Parse(t)
	if err != nil || u.Hostname() == "" {
		return "", "That is not an address like host:port"
	}
	switch u.Scheme {
	case "http", "ws":
		u.Scheme = "ws"
	case "https", "wss":
		u.Scheme = "wss"
	default:
		return "", "Use host:port or an http:// address"
	}
	port := u.Port()
	if port == "" {
		port = defaultPort
	}
	if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
		return "", "The port must be a number from 1 to 65535"
	}
	u.Host = net.JoinHostPort(u.Hostname(), port)
	if u.Path == "" || u.Path == "/" {
		u.Path = "/panel"
	}
	u.RawQuery, u.Fragment, u.User = "", "", nil
	return u.String(), ""
}

// shortAddr shows ws://host:port/panel as host:port.
func shortAddr(ws string) string {
	return strings.TrimSuffix(strings.TrimPrefix(ws, "ws://"), "/panel")
}

// ---- the saved choice ----

func envQuote(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
}

// saveChoice makes the choice permanent. With switchNow, the current session ends so the panel
// connects to the new renderer right away.
func (a *app) saveChoice(c choice, switchNow bool) error {
	text := "# Written by desk-panel: PANEL_FIND = a renderer found on the network, PANEL_ADDR = a typed address.\n"
	if c.find != "" {
		text += "PANEL_FIND=" + envQuote(c.find) + "\n"
	}
	if c.addr != "" {
		text += "PANEL_ADDR=" + envQuote(c.addr) + "\n"
	}
	tmp := a.configPath + ".tmp"
	if err := os.WriteFile(tmp, []byte(text), 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, a.configPath); err != nil {
		return err
	}
	a.mu.Lock()
	a.choice = c
	a.mu.Unlock()
	log.Printf("renderer choice: find=%q addr=%q", c.find, c.addr)
	if switchNow {
		select {
		case a.reconnect <- struct{}{}:
		default:
		}
	}
	return nil
}

// resolve turns the choice into a stream URL, looking on the LAN when needed. "" = nothing to
// connect to yet (the status line says why).
func (a *app) resolve() (string, string) {
	c := a.current()
	if c.addr != "" {
		return c.addr, shortAddr(c.addr)
	}
	found, err := findRenderers(2 * time.Second)
	if err != nil {
		a.setStatus("Cannot look for renderers: " + err.Error())
		return "", ""
	}
	a.setFound(found)
	if c.find != "" {
		for _, r := range found {
			if r.Name != c.find {
				continue
			}
			if u := r.url(); u != "" {
				return u, r.Name
			}
			a.setStatus(r.Name + " only offers a secure address, which this panel cannot use yet")
			return "", ""
		}
		a.setStatus("Looking for " + c.find + " on your network…")
		return "", ""
	}
	var usable []renderer
	for _, r := range found {
		if r.url() != "" {
			usable = append(usable, r)
		}
	}
	switch len(usable) {
	case 0:
		a.setStatus("Looking for a renderer on your network…")
		return "", ""
	case 1:
		// The only renderer around: take it and remember it by name.
		r := usable[0]
		if err := a.saveChoice(choice{find: r.Name}, false); err != nil {
			log.Printf("cannot save the choice: %v", err)
		}
		a.mu.Lock()
		ui := a.ui
		a.mu.Unlock()
		if ui != nil && ui.untouched() {
			ui.close()
		}
		return r.url(), r.Name
	default:
		a.setStatus(fmt.Sprintf("%d renderers found: pick one", len(usable)))
		a.mu.Lock()
		open := a.ui != nil
		a.mu.Unlock()
		if !open {
			a.openOverlay(true)
		}
		return "", ""
	}
}

// ---- touches ----

// route sends touches to the address screen while it is open, otherwise to the renderer.
func (a *app) route(touches <-chan touch) {
	forward := func(t touch) {
		select {
		case a.send <- t:
		default: // no session right now: drop
		}
	}
	// The first finger drives the page's mouse. If the swipe's first finger pressed the page,
	// release it there, because the rest of the swipe goes to the address screen.
	var first touch
	firstDown := false
	for t := range touches {
		a.mu.Lock()
		ui := a.ui
		a.mu.Unlock()
		switch {
		case t.kind == kindGesture && ui == nil:
			if firstDown {
				forward(touch{kindUp, 0, first.x, first.y})
				firstDown = false
			}
			a.openOverlay(false)
		case t.kind == kindGesture:
		case ui != nil:
			ui.touch(t)
		default:
			if t.finger == 0 {
				first, firstDown = t, t.kind != kindUp
			}
			forward(t)
		}
	}
}

// ---- the session ----

func (a *app) session(streamURL, label string) error {
	dialer := websocket.Dialer{
		HandshakeTimeout: 5 * time.Second,
		NetDialContext:   (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 10 * time.Second}).DialContext,
	}
	conn, _, err := dialer.Dial(streamURL, nil)
	if err != nil {
		return err
	}
	defer conn.Close()
	conn.SetReadLimit(32 << 20)
	// The renderer pings every 15 s; silence for longer than this means the link is gone.
	alive := func() { conn.SetReadDeadline(time.Now().Add(45 * time.Second)) }
	alive()
	conn.SetPingHandler(func(p string) error {
		alive()
		return conn.WriteControl(websocket.PongMessage, []byte(p), time.Now().Add(5*time.Second))
	})
	host, _ := os.Hostname()
	hello := map[string]any{"type": "hello", "proto": 1, "width": a.s.w, "height": a.s.h, "name": host}
	if err := conn.WriteJSON(hello); err != nil {
		return err
	}
	log.Printf("connected to %s (%s)", label, streamURL)
	a.setStatus("Showing " + label)
	for len(a.send) > 0 { // touches from while we were offline are stale
		<-a.send
	}

	failed := make(chan error, 1)
	go func() {
		var pixels []byte
		for {
			kind, data, err := conn.ReadMessage()
			if err != nil {
				failed <- err
				return
			}
			alive()
			if kind == websocket.TextMessage {
				var m struct {
					Type          string
					Width, Height int
				}
				if json.Unmarshal(data, &m) == nil && m.Type == "size" && (m.Width != a.s.w || m.Height != a.s.h) {
					log.Printf("the page is %dx%d, this screen %dx%d: showing what fits", m.Width, m.Height, a.s.w, a.s.h)
				}
				continue
			}
			if err := a.s.applyFrame(data, &pixels); err != nil {
				failed <- err
				return
			}
			stats.Lock()
			stats.frames++
			stats.bytes += len(data)
			stats.pixels += int(binary.LittleEndian.Uint16(data[4:])) * int(binary.LittleEndian.Uint16(data[6:]))
			stats.Unlock()
		}
	}()

	msg := make([]byte, 6)
	accent := rgb(0xff, 0xd4, 0x79)
	for {
		select {
		case err := <-failed:
			return err
		case <-a.reconnect:
			return errSwitch
		case t := <-a.send:
			if t.kind == kindDown {
				a.s.Lock()
				a.s.mark(t.x, t.y, ringRadius, 4, accent)
				a.s.Unlock()
				x, y := t.x, t.y
				time.AfterFunc(ringFor, func() {
					a.s.Lock()
					a.s.restore(x, y, ringRadius)
					a.s.Unlock()
				})
			}
			msg[0], msg[1] = t.kind, t.finger
			binary.LittleEndian.PutUint16(msg[2:], uint16(t.x))
			binary.LittleEndian.PutUint16(msg[4:], uint16(t.y))
			conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
			if err := conn.WriteMessage(websocket.BinaryMessage, msg); err != nil {
				return err
			}
		}
	}
}

// ---- main ----

func listRenderers() {
	found, err := findRenderers(3 * time.Second)
	if err != nil {
		log.Fatal(err)
	}
	if len(found) == 0 {
		fmt.Println("no renderer found (_desk-panel._tcp)")
	}
	for _, r := range found {
		fmt.Printf("%s\n  host %s  ip %v  port %d  txt %v\n  stream %s\n", r.Name, r.Host, r.IP, r.Port, r.TXT, r.url())
	}
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "-find" {
		listRenderers()
		return
	}
	a := &app{
		configPath: os.Getenv("PANEL_CONFIG"),
		send:       make(chan touch, 64),
		reconnect:  make(chan struct{}, 1),
		art:        make(chan *art, 1),
		status:     "Not connected yet",
	}
	if a.configPath == "" {
		a.configPath = "/etc/default/desk-panel"
	}
	a.choice.find = os.Getenv("PANEL_FIND")
	if addr := os.Getenv("PANEL_ADDR"); addr != "" && a.choice.find == "" {
		if u, problem := normalizeAddr(addr); problem == "" {
			a.choice.addr = u
		} else {
			log.Printf("ignoring PANEL_ADDR %q: %s", addr, problem)
		}
	}
	s, err := openScreen()
	if err != nil {
		log.Fatal(err)
	}
	a.s = s
	go func() {
		a.fonts = loadFonts()
		a.art <- buildArt(a)
	}()

	// Keep the text console from drawing over the panel; give it back on exit.
	if tty, err := os.OpenFile("/dev/tty0", os.O_RDWR, 0); err == nil {
		syscall.Syscall(syscall.SYS_IOCTL, tty.Fd(), kdSetMode, kdGraphics)
		stop := make(chan os.Signal, 1)
		signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
		go func() {
			<-stop
			syscall.Syscall(syscall.SYS_IOCTL, tty.Fd(), kdSetMode, kdText)
			os.Exit(0)
		}()
	}
	s.Lock()
	s.fill(rgb(0x10, 0x14, 0x18))
	s.Unlock()

	touches := make(chan touch, 64)
	go readTouches(s, touches)
	go a.route(touches)
	go reportStats()

	if c := a.current(); c.addr == "" && c.find == "" {
		a.openOverlay(true) // first start: show what the panel finds while it looks
	}

	// While the renderer is unreachable, a small dot in the corner says so.
	offline := rgb(0x9f, 0xb3, 0xc8)
	for {
		streamURL, label := a.resolve()
		var err error
		if streamURL == "" {
			err = errors.New("not found")
		} else {
			err = a.session(streamURL, label)
			if errors.Is(err, errSwitch) {
				continue
			}
			log.Printf("renderer %s: %v", label, err)
			a.setStatus("Cannot reach " + label)
		}
		s.Lock()
		s.mark(s.w-16, s.h-16, 6, 6, offline)
		s.Unlock()
		select {
		case <-time.After(2 * time.Second):
		case <-a.reconnect:
		}
		s.Lock()
		s.restore(s.w-16, s.h-16, 6)
		s.Unlock()
	}
}
