// Panel client for the Pi: draws screen updates streamed by the renderer straight into /dev/fb0 and
// sends touches back. Shows an instant ring where a finger lands, before the page has reacted.
// Finds renderers on the LAN by itself (mdns.go); a three-finger swipe down opens the address screen
// (address.go). Wire format: see ../app/server.mjs.
//
// Run `desk-panel -find` to list the renderers this Pi can see.
package main

import (
	"bufio"
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"fmt"
	"io"
	"log"
	"os"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

const (
	evSyn           = 0
	evAbs           = 3
	absMtSlot       = 0x2f
	absMtX          = 0x35
	absMtY          = 0x36
	absMtTrackingID = 0x39
	kdSetMode       = 0x4B3A
	kdText          = 0
	kdGraphics      = 1
	eventSize       = 16 // struct input_event on 32-bit ARM
	maxSlots        = 10
	moveEvery       = 30 * time.Millisecond
	ringRadius      = 22
	ringFor         = 180 * time.Millisecond
	swipeDistance   = 120 // pixels every finger must travel down for the three-finger gesture
)

// Touch kinds. The first three go to the renderer; kindGesture stays on the Pi.
const (
	kindDown    = 1
	kindMove    = 2
	kindUp      = 3
	kindGesture = 9
)

// screen is the mapped framebuffer plus a copy of the last frame from the renderer, so local marks
// (the tap ring, the offline dot) can be undone. While covered, the address screen owns the display
// and renderer frames only update the copy.
type screen struct {
	sync.Mutex
	mem, shadow  []byte
	w, h, stride int
	covered      bool
}

func readSysfs(name string) string {
	b, _ := os.ReadFile("/sys/class/graphics/fb0/" + name)
	return strings.TrimSpace(string(b))
}

func openScreen() (*screen, error) {
	size := strings.Split(readSysfs("virtual_size"), ",")
	if len(size) != 2 {
		return nil, fmt.Errorf("cannot read framebuffer size")
	}
	w, _ := strconv.Atoi(size[0])
	h, _ := strconv.Atoi(size[1])
	stride, _ := strconv.Atoi(readSysfs("stride"))
	if bpp := readSysfs("bits_per_pixel"); bpp != "16" {
		return nil, fmt.Errorf("expected a 16-bit framebuffer, got %s", bpp)
	}
	f, err := os.OpenFile("/dev/fb0", os.O_RDWR, 0)
	if err != nil {
		return nil, err
	}
	mem, err := syscall.Mmap(int(f.Fd()), 0, stride*h, syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED)
	if err != nil {
		return nil, err
	}
	return &screen{mem: mem, shadow: make([]byte, stride*h), w: w, h: h, stride: stride}, nil
}

func rgb(r, g, b uint8) uint16 { return uint16(r>>3)<<11 | uint16(g>>2)<<5 | uint16(b>>3) }

// fill paints the whole screen and the shadow copy. Caller holds the lock.
func (s *screen) fill(c uint16) {
	for i := 0; i+1 < len(s.shadow); i += 2 {
		s.shadow[i], s.shadow[i+1] = byte(c), byte(c>>8)
	}
	if !s.covered {
		copy(s.mem, s.shadow)
	}
}

// mark paints a ring onto the screen only (not the shadow). Caller holds the lock.
func (s *screen) mark(cx, cy, r, t int, c uint16) {
	if s.covered {
		return
	}
	for y := max(cy-r, 0); y <= min(cy+r, s.h-1); y++ {
		for x := max(cx-r, 0); x <= min(cx+r, s.w-1); x++ {
			d := (x-cx)*(x-cx) + (y-cy)*(y-cy)
			if d <= r*r && d >= (r-t)*(r-t) {
				i := y*s.stride + x*2
				s.mem[i], s.mem[i+1] = byte(c), byte(c>>8)
			}
		}
	}
}

// restore copies the shadow back over a square area, undoing marks. Caller holds the lock.
func (s *screen) restore(cx, cy, r int) {
	if s.covered {
		return
	}
	x0, x1 := max(cx-r, 0), min(cx+r, s.w-1)
	for y := max(cy-r, 0); y <= min(cy+r, s.h-1); y++ {
		a, b := y*s.stride+x0*2, y*s.stride+x1*2+2
		copy(s.mem[a:b], s.shadow[a:b])
	}
}

// uncover hands the display back to the renderer's picture.
func (s *screen) uncover() {
	s.Lock()
	s.covered = false
	copy(s.mem, s.shadow)
	s.Unlock()
}

// applyFrame draws one update from the renderer: a 12-byte header, then zlib RGB565 pixels.
// A page larger than this screen is clipped to what fits.
func (s *screen) applyFrame(msg []byte, pixels *[]byte) error {
	if len(msg) < 12 {
		return fmt.Errorf("short frame (%d bytes)", len(msg))
	}
	work := time.Now()
	defer func() {
		stats.Lock()
		stats.work += time.Since(work)
		stats.Unlock()
	}()
	x := int(binary.LittleEndian.Uint16(msg[0:]))
	y := int(binary.LittleEndian.Uint16(msg[2:]))
	w := int(binary.LittleEndian.Uint16(msg[4:]))
	h := int(binary.LittleEndian.Uint16(msg[6:]))
	if w == 0 || h == 0 || w*h > 4096*4096 {
		return fmt.Errorf("bad frame %d,%d %dx%d", x, y, w, h)
	}
	zr, err := zlib.NewReader(bytes.NewReader(msg[12:]))
	if err != nil {
		return err
	}
	if cap(*pixels) < w*h*2 {
		*pixels = make([]byte, w*h*2)
	}
	px := (*pixels)[:w*h*2]
	if _, err := io.ReadFull(zr, px); err != nil {
		return err
	}
	cw, ch := min(w, s.w-x), min(h, s.h-y)
	if cw <= 0 || ch <= 0 {
		return nil
	}
	s.Lock()
	defer s.Unlock()
	for row := 0; row < ch; row++ {
		at := (y+row)*s.stride + x*2
		line := px[row*w*2 : row*w*2+cw*2]
		copy(s.shadow[at:at+cw*2], line)
		if !s.covered {
			copy(s.mem[at:at+cw*2], line)
		}
	}
	return nil
}

// ---- measurements ----

var stats struct {
	sync.Mutex
	frames, bytes, pixels int
	work                  time.Duration
}

// cpuTicks is this process's user+system time in clock ticks (100 per second).
func cpuTicks() int {
	b, _ := os.ReadFile("/proc/self/stat")
	f := strings.Fields(string(b[strings.LastIndexByte(string(b), ')')+1:]))
	if len(f) < 13 {
		return 0
	}
	u, _ := strconv.Atoi(f[11])
	k, _ := strconv.Atoi(f[12])
	return u + k
}

func reportStats() {
	const every = 5 * time.Second
	last := cpuTicks()
	for range time.Tick(every) {
		now := cpuTicks()
		stats.Lock()
		if stats.frames > 0 {
			log.Printf("panel: %.1f frames/s, %.0f KB/s, %.0f%% of screen per frame, %.1f ms work per frame, cpu %d%%",
				float64(stats.frames)/every.Seconds(), float64(stats.bytes)/1024/every.Seconds(),
				float64(stats.pixels)/float64(stats.frames)/(1024*600)*100,
				float64(stats.work.Microseconds())/float64(stats.frames)/1000, (now-last)*100/int(every.Seconds()*100))
		}
		stats.frames, stats.bytes, stats.pixels, stats.work = 0, 0, 0, 0
		stats.Unlock()
		last = now
	}
}

// ---- touch ----

type touch struct {
	kind, finger byte
	x, y         int
}

func hasBit(bitmap string, bit int) bool {
	words := strings.Fields(bitmap)
	i := len(words) - 1 - bit/32
	if i < 0 {
		return false
	}
	w, _ := strconv.ParseUint(words[i], 16, 64)
	return w>>(bit%32)&1 == 1
}

func touchDevice() string {
	text, _ := os.ReadFile("/proc/bus/input/devices")
	for _, block := range strings.Split(string(text), "\n\n") {
		var handlers, abs string
		for _, line := range strings.Split(block, "\n") {
			if strings.HasPrefix(line, "H: Handlers=") {
				handlers = strings.TrimPrefix(line, "H: Handlers=")
			}
			if strings.HasPrefix(line, "B: ABS=") {
				abs = strings.TrimPrefix(line, "B: ABS=")
			}
		}
		if !hasBit(abs, absMtX) {
			continue
		}
		for _, f := range strings.Fields(handlers) {
			if strings.HasPrefix(f, "event") {
				return "/dev/input/" + f
			}
		}
	}
	return ""
}

func absRange(fd uintptr, code int) (int32, int32) {
	var info [6]int32 // value, minimum, maximum, fuzz, flat, resolution
	syscall.Syscall(syscall.SYS_IOCTL, fd, uintptr(0x80184540)+uintptr(code), uintptr(unsafe.Pointer(&info[0])))
	return info[1], info[2]
}

// readTouches follows the first multitouch device, reopening it if the screen is unplugged.
// It reports each finger's down, moves (throttled) and up, plus one kindGesture when three or
// more fingers all travel swipeDistance downward together.
func readTouches(s *screen, out chan<- touch) {
	type slot struct {
		active, down, moved bool
		x, y                int32
		startY              int
		sent                time.Time
	}
	for {
		path := touchDevice()
		f, err := os.Open(path)
		if path == "" || err != nil {
			time.Sleep(2 * time.Second)
			continue
		}
		minX, maxX := absRange(f.Fd(), absMtX)
		minY, maxY := absRange(f.Fd(), absMtY)
		log.Printf("touch: %s x %d..%d y %d..%d", path, minX, maxX, minY, maxY)
		scale := func(v, lo, hi int32, size int) int {
			if hi <= lo {
				return int(v)
			}
			return min(max(int(v-lo)*(size-1)/int(hi-lo), 0), size-1)
		}
		var slots [maxSlots]slot
		cur := 0
		gestured := false
		r := bufio.NewReaderSize(f, eventSize*64)
		buf := make([]byte, eventSize)
		for {
			if _, err := io.ReadFull(r, buf); err != nil {
				log.Printf("touch device gone: %v", err)
				break
			}
			typ := binary.LittleEndian.Uint16(buf[8:])
			code := binary.LittleEndian.Uint16(buf[10:])
			val := int32(binary.LittleEndian.Uint32(buf[12:]))
			sl := &slots[cur]
			switch {
			case typ == evAbs && code == absMtSlot && val >= 0 && val < maxSlots:
				cur = int(val)
			case typ == evAbs && code == absMtTrackingID && val >= 0:
				sl.active, sl.down = true, false
			case typ == evAbs && code == absMtTrackingID:
				if sl.down {
					out <- touch{kindUp, byte(cur), scale(sl.x, minX, maxX, s.w), scale(sl.y, minY, maxY, s.h)}
				}
				sl.active, sl.down = false, false
			case typ == evAbs && code == absMtX:
				sl.x, sl.moved = val, true
			case typ == evAbs && code == absMtY:
				sl.y, sl.moved = val, true
			case typ == evSyn && code == 0:
				fingers, allSwiped := 0, true
				for i := range slots {
					p := &slots[i]
					if !p.active {
						continue
					}
					x, y := scale(p.x, minX, maxX, s.w), scale(p.y, minY, maxY, s.h)
					if p.moved {
						if !p.down {
							p.down, p.startY, p.sent = true, y, time.Now()
							out <- touch{kindDown, byte(i), x, y}
						} else if time.Since(p.sent) >= moveEvery {
							p.sent = time.Now()
							out <- touch{kindMove, byte(i), x, y}
						}
						p.moved = false
					}
					if p.down {
						fingers++
						if y-p.startY < swipeDistance {
							allSwiped = false
						}
					}
				}
				if fingers >= 3 && allSwiped && !gestured {
					gestured = true
					out <- touch{kind: kindGesture}
				}
				if fingers == 0 {
					gestured = false
				}
			}
		}
		f.Close()
	}
}
