package main

import (
	"image"
	"image/color"
	"image/draw"
	"log"
	"strings"
	"sync"
	"time"
)

// ---- pre-drawn screen ----

type key struct {
	label  string
	action string // "char", "delete", "clear", "cancel", "connect", "pick"
	r      image.Rectangle
	index  int // "pick": which found renderer
}

type art struct {
	normal, pressed      *image.RGBA
	keys                 []key
	found, field, status image.Rectangle
}

func buildArt(a *app) *art {
	started := time.Now()
	w, h := a.s.w, a.s.h
	t := &art{
		normal:  image.NewRGBA(image.Rect(0, 0, w, h)),
		pressed: image.NewRGBA(image.Rect(0, 0, w, h)),
		found:   image.Rect(40, 100, w-40, 156),
		field:   image.Rect(40, 168, w-40, 224),
		status:  image.Rect(40, 228, w-40, 268),
	}
	rows := []string{"1234567890", "qwertyuiop", "asdfghjkl:", "zxcvbnm.-/"}
	const gap, keyH = 6, 56
	keyW := (w - 2*12 - 9*gap) / 10
	x0 := (w - (10*keyW + 9*gap)) / 2
	y := h - 5*(keyH+gap) - 8
	for _, row := range rows {
		for i, ch := range row {
			x := x0 + i*(keyW+gap)
			t.keys = append(t.keys, key{label: string(ch), action: "char", r: image.Rect(x, y, x+keyW, y+keyH)})
		}
		y += keyH + gap
	}
	span := func(from, n int) image.Rectangle {
		x := x0 + from*(keyW+gap)
		return image.Rect(x, y, x+n*keyW+(n-1)*gap, y+keyH)
	}
	t.keys = append(t.keys,
		key{label: "Delete", action: "delete", r: span(0, 2)},
		key{label: "Clear", action: "clear", r: span(2, 2)},
		key{label: "Cancel", action: "cancel", r: span(4, 2)},
		key{label: "Connect", action: "connect", r: span(6, 4)},
	)

	f := a.fonts
	for i, img := range []*image.RGBA{t.normal, t.pressed} {
		draw.Draw(img, img.Rect, image.NewUniform(colBg), image.Point{}, draw.Src)
		f.large.draw(img, 40, 56, "Choose a renderer", colText)
		f.small.draw(img, 40, 84, "Tap one found on your network, or type its address. Three fingers down brings you back.", colMuted)
		for _, k := range t.keys {
			bg, fg := colKey, colText
			if k.action == "connect" {
				bg, fg = colAccent, colBg
			}
			if i == 1 {
				bg = colPressed
			}
			draw.Draw(img, k.r, image.NewUniform(bg), image.Point{}, draw.Src)
			kf := f.normal
			if k.action != "char" {
				kf = f.small
			}
			kf.centered(img, k.r, k.label, fg)
		}
	}
	// Warm the caches for the address field and the found list.
	f.large.width("0123456789abcdefghijklmnopqrstuvwxyz:.-/")
	f.small.width("ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789 ·#()-…")
	log.Printf("address screen ready in %s", time.Since(started).Round(time.Millisecond))
	return t
}

// ---- the open screen ----

type overlay struct {
	a         *app
	art       *art
	automatic bool // opened by the panel itself, not by a swipe

	mu        sync.Mutex
	canvas    *image.RGBA
	text      string
	touched   bool
	closed    bool
	found     []renderer
	foundKeys []key
}

// openOverlay shows the address screen. automatic = the panel opened it itself (first start, or
// several renderers to choose from); it then closes on its own when the only renderer turns up,
// unless someone has started using it.
func (a *app) openOverlay(automatic bool) {
	started := time.Now()
	t := <-a.art // built at startup; waits only if the screen is wanted before it is ready
	a.art <- t
	o := &overlay{a: a, art: t, automatic: automatic, canvas: image.NewRGBA(t.normal.Rect), text: a.address()}
	copy(o.canvas.Pix, t.normal.Pix)
	a.mu.Lock()
	if a.ui != nil {
		a.mu.Unlock()
		return
	}
	a.ui = o
	status, found := a.status, a.found
	a.mu.Unlock()
	a.s.Lock()
	a.s.covered = true
	a.s.Unlock()
	o.mu.Lock()
	o.blit(o.canvas.Rect)
	o.drawField()
	o.drawStatus(status, colMuted)
	o.drawFound(found)
	o.mu.Unlock()
	log.Printf("address screen opened in %s", time.Since(started).Round(time.Millisecond))

	// Keep the found list fresh while the screen is open.
	go func() {
		for !o.isClosed() {
			if found, err := findRenderers(1500 * time.Millisecond); err == nil && !o.isClosed() {
				a.setFound(found)
			}
			time.Sleep(2 * time.Second)
		}
	}()
}

func (o *overlay) isClosed() bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.closed
}

// untouched: opened by the panel and nobody has used it yet.
func (o *overlay) untouched() bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.automatic && !o.touched
}

func (o *overlay) close() {
	o.mu.Lock()
	o.closed = true
	o.mu.Unlock()
	o.a.mu.Lock()
	if o.a.ui == o {
		o.a.ui = nil
	}
	o.a.mu.Unlock()
	o.a.s.uncover()
}

// blit converts part of the canvas to RGB565 and puts it on the display. Caller holds o.mu.
func (o *overlay) blit(r image.Rectangle) {
	s := o.a.s
	r = r.Intersect(o.canvas.Rect)
	s.Lock()
	defer s.Unlock()
	if !s.covered || o.closed {
		return
	}
	for y := r.Min.Y; y < r.Max.Y; y++ {
		src := o.canvas.Pix[y*o.canvas.Stride:]
		dst := s.mem[y*s.stride:]
		for x := r.Min.X; x < r.Max.X; x++ {
			p := src[x*4:]
			v := uint16(p[0]>>3)<<11 | uint16(p[1]>>2)<<5 | uint16(p[2]>>3)
			dst[x*2], dst[x*2+1] = byte(v), byte(v>>8)
		}
	}
}

func (o *overlay) drawKey(k key, pressed bool) {
	if k.action == "pick" {
		o.drawFoundKey(k, pressed)
		return
	}
	from := o.art.normal
	if pressed {
		from = o.art.pressed
	}
	draw.Draw(o.canvas, k.r, from, k.r.Min, draw.Src)
	o.blit(k.r)
}

func (o *overlay) drawField() {
	f := o.a.fonts.large
	r := o.art.field
	draw.Draw(o.canvas, r, image.NewUniform(colField), image.Point{}, draw.Src)
	shown := o.text
	for f.width(shown) > r.Dx()-48 && len(shown) > 0 { // keep the end of a long address visible
		shown = shown[1:]
	}
	baseline := r.Min.Y + (r.Dy()+f.ascent-f.descent)/2
	end := f.draw(o.canvas, r.Min.X+16, baseline, shown, colText)
	draw.Draw(o.canvas, image.Rect(end+3, r.Min.Y+12, end+6, r.Max.Y-12), image.NewUniform(colAccent), image.Point{}, draw.Src)
	o.blit(r)
}

func (o *overlay) drawStatus(text string, c color.RGBA) {
	r := o.art.status
	draw.Draw(o.canvas, r, image.NewUniform(colBg), image.Point{}, draw.Src)
	o.a.fonts.small.draw(o.canvas, r.Min.X, r.Min.Y+28, text, c)
	o.blit(r)
}

func (o *overlay) showStatus(text string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.drawStatus(text, colMuted)
}

// ---- found renderers ----

func foundLabel(r renderer) string {
	host := strings.TrimSuffix(strings.TrimSuffix(r.Host, "."), ".local")
	if host == "" || strings.Contains(strings.ToLower(r.Name), strings.ToLower(host)) {
		return r.Name
	}
	return r.Name + " · " + host
}

// fit shortens text with an ellipsis until it fits width.
func fit(f *face, text string, width int) string {
	if f.width(text) <= width {
		return text
	}
	runes := []rune(text)
	for len(runes) > 0 && f.width(string(runes)+"…") > width {
		runes = runes[:len(runes)-1]
	}
	return string(runes) + "…"
}

func (o *overlay) drawFoundKey(k key, pressed bool) {
	r := o.found[k.index]
	bg, fg := colKey, colText
	if c := o.a.current(); c.addr == "" && c.find == r.Name {
		bg, fg = colAccent, colBg // the one this panel shows
	}
	if pressed {
		bg, fg = colPressed, colText
	}
	draw.Draw(o.canvas, k.r, image.NewUniform(bg), image.Point{}, draw.Src)
	f := o.a.fonts.small
	f.centered(o.canvas, k.r, fit(f, k.label, k.r.Dx()-24), fg)
	o.blit(k.r)
}

// drawFound lays out up to three found renderers as buttons. Caller holds o.mu.
func (o *overlay) drawFound(found []renderer) {
	r := o.art.found
	draw.Draw(o.canvas, r, image.NewUniform(colBg), image.Point{}, draw.Src)
	o.found, o.foundKeys = found, nil
	if len(found) == 0 {
		o.a.fonts.small.draw(o.canvas, r.Min.X, r.Min.Y+36, "No renderer found on your network yet.", colMuted)
		o.blit(r)
		return
	}
	const gap = 12
	bw := (r.Dx() - 2*gap) / 3
	for i := range found[:min(len(found), 3)] {
		x := r.Min.X + i*(bw+gap)
		k := key{label: foundLabel(found[i]), action: "pick", r: image.Rect(x, r.Min.Y, x+bw, r.Max.Y), index: i}
		o.foundKeys = append(o.foundKeys, k)
		o.drawFoundKey(k, false)
	}
	o.blit(r)
}

func (o *overlay) showFound(found []renderer) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.closed {
		return
	}
	// Keep the buttons still while nothing changed (avoids flicker under a finger).
	if len(found) == len(o.found) {
		same := true
		for i := range found {
			if found[i].Name != o.found[i].Name || found[i].Host != o.found[i].Host {
				same = false
			}
		}
		if same {
			o.found = found
			return
		}
	}
	o.drawFound(found)
}

// ---- input ----

func (o *overlay) touch(t touch) {
	if t.kind != kindDown {
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	o.touched = true
	keys := append(append([]key{}, o.foundKeys...), o.art.keys...)
	for _, k := range keys {
		if !(image.Point{t.x, t.y}).In(k.r) {
			continue
		}
		o.drawKey(k, true)
		k := k
		found := o.found
		time.AfterFunc(120*time.Millisecond, func() {
			o.mu.Lock()
			defer o.mu.Unlock()
			if k.action != "pick" || (k.index < len(o.found) && o.found[k.index].Name == found[k.index].Name) {
				o.drawKey(k, false)
			}
		})
		switch k.action {
		case "pick":
			r := o.found[k.index]
			if r.url() == "" {
				o.drawStatus(r.Name+" only offers a secure address, which this panel cannot use yet", colError)
				return
			}
			if err := o.a.saveChoice(choice{find: r.Name}, true); err != nil {
				o.drawStatus("Could not save: "+err.Error(), colError)
				return
			}
			go o.close()
		case "char":
			if len(o.text) < 120 {
				o.text += k.label
			}
			o.drawField()
		case "delete":
			if len(o.text) > 0 {
				o.text = o.text[:len(o.text)-1]
			}
			o.drawField()
		case "clear":
			o.text = ""
			o.drawField()
		case "cancel":
			go o.close()
		case "connect":
			addr, problem := normalizeAddr(o.text)
			if problem != "" {
				o.drawStatus(problem, colError)
				return
			}
			if err := o.a.saveChoice(choice{addr: addr}, true); err != nil {
				o.drawStatus("Could not save: "+err.Error(), colError)
				return
			}
			go o.close()
		}
		return
	}
}
