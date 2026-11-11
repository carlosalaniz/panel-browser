// The address screen: opened by a three-finger swipe down (or at start when nothing is chosen yet).
// Lists the renderers found on the network (tap one to show it) and has an on-screen keyboard to
// type an address instead; Connect saves it and switches over.
//
// Rasterizing glyphs is slow on the Pi Zero's ARMv6 core, so every glyph is drawn once and cached,
// and the static screen (keys up and keys pressed) is pre-drawn in the background at startup.
// Opening the screen then only copies ready pixels.
package main

import (
	"image"
	"image/color"
	"image/draw"
	"log"
	"os"
	"sync"

	"golang.org/x/image/font"
	"golang.org/x/image/font/basicfont"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/math/fixed"
)

var (
	colBg      = color.RGBA{0x10, 0x14, 0x18, 0xff}
	colText    = color.RGBA{0xf2, 0xf4, 0xf7, 0xff}
	colMuted   = color.RGBA{0x9f, 0xb3, 0xc8, 0xff}
	colKey     = color.RGBA{0x26, 0x30, 0x3a, 0xff}
	colPressed = color.RGBA{0x45, 0x55, 0x66, 0xff}
	colField   = color.RGBA{0x1a, 0x21, 0x29, 0xff}
	colAccent  = color.RGBA{0xff, 0xd4, 0x79, 0xff}
	colError   = color.RGBA{0xff, 0x7b, 0x72, 0xff}
)

// ---- cached text ----

type glyph struct {
	mask *image.Alpha
	off  image.Point // top-left of the mask relative to the pen position on the baseline
	adv  int
}

type face struct {
	f               font.Face
	ascent, descent int
	mu              sync.Mutex
	glyphs          map[rune]glyph
}

func newFace(f font.Face) *face {
	m := f.Metrics()
	return &face{f: f, ascent: m.Ascent.Ceil(), descent: m.Descent.Ceil(), glyphs: map[rune]glyph{}}
}

func (c *face) get(r rune) glyph {
	c.mu.Lock()
	defer c.mu.Unlock()
	if g, ok := c.glyphs[r]; ok {
		return g
	}
	dr, mask, mp, adv, ok := c.f.Glyph(fixed.P(0, 0), r)
	g := glyph{adv: adv.Round()}
	if ok && !dr.Empty() {
		// The face reuses its mask buffer, so keep a copy.
		g.mask = image.NewAlpha(image.Rect(0, 0, dr.Dx(), dr.Dy()))
		draw.Draw(g.mask, g.mask.Rect, mask, mp, draw.Src)
		g.off = dr.Min
	}
	c.glyphs[r] = g
	return g
}

func (c *face) width(text string) int {
	w := 0
	for _, r := range text {
		w += c.get(r).adv
	}
	return w
}

// draw writes text with its baseline at y and returns the x where it ended.
func (c *face) draw(dst *image.RGBA, x, y int, text string, col color.RGBA) int {
	src := image.NewUniform(col)
	for _, r := range text {
		g := c.get(r)
		if g.mask != nil {
			at := image.Pt(x, y).Add(g.off)
			draw.DrawMask(dst, g.mask.Rect.Add(at), src, image.Point{}, g.mask, image.Point{}, draw.Over)
		}
		x += g.adv
	}
	return x
}

func (c *face) centered(dst *image.RGBA, r image.Rectangle, text string, col color.RGBA) {
	baseline := r.Min.Y + (r.Dy()+c.ascent-c.descent)/2
	c.draw(dst, r.Min.X+(r.Dx()-c.width(text))/2, baseline, text, col)
}

type fonts struct{ large, normal, small *face }

func loadFonts() *fonts {
	data, err := os.ReadFile("/usr/share/fonts/truetype/dejavu/DejaVuSans.ttf")
	if err == nil {
		var f *opentype.Font
		if f, err = opentype.Parse(data); err == nil {
			size := func(pt float64) *face {
				fc, _ := opentype.NewFace(f, &opentype.FaceOptions{Size: pt, DPI: 72, Hinting: font.HintingFull})
				return newFace(fc)
			}
			return &fonts{large: size(34), normal: size(26), small: size(20)}
		}
	}
	log.Printf("DejaVu Sans not available (%v), using the built-in bitmap font", err)
	b := newFace(basicfont.Face7x13)
	return &fonts{large: b, normal: b, small: b}
}
