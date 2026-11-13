package avct

import (
	"image"
	"image/color"
	"sync"
)

// Framebuffer is the console's video surface: RGBA pixels plus dirty-rect
// accumulation that the RFB bridge drains into noVNC updates. All video
// decoders (DVC, ASpeed, textmode) draw into it.
type Framebuffer struct {
	mu    sync.Mutex
	rgba  *image.RGBA
	dirty image.Rectangle
}

// NewFramebuffer allocates a black surface of the given size.
func NewFramebuffer(w, h int) *Framebuffer {
	fb := &Framebuffer{rgba: image.NewRGBA(image.Rect(0, 0, max2(w, 1), max2(h, 1)))}
	fb.fill(color.RGBA{0, 0, 0, 255})
	return fb
}

func max2(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func (fb *Framebuffer) fill(c color.RGBA) {
	b := fb.rgba.Bounds()
	for y := b.Min.Y; y < b.Max.Y; y++ {
		row := fb.rgba.Pix[y*fb.rgba.Stride : y*fb.rgba.Stride+b.Dx()*4]
		for x := 0; x < b.Dx(); x++ {
			row[x*4], row[x*4+1], row[x*4+2], row[x*4+3] = c.R, c.G, c.B, c.A
		}
	}
	fb.dirty = image.Rect(0, 0, b.Dx(), b.Dy())
}

// Resize swaps the surface (resolution change), preserving nothing — the
// appliance redraws everything after its own mode change.
func (fb *Framebuffer) Resize(w, h int) {
	fb.mu.Lock()
	defer fb.mu.Unlock()
	if w <= 0 || h <= 0 {
		return
	}
	fb.rgba = image.NewRGBA(image.Rect(0, 0, w, h))
	fb.fill(color.RGBA{0, 0, 0, 255})
}

// Size returns the current geometry.
func (fb *Framebuffer) Size() (int, int) {
	fb.mu.Lock()
	defer fb.mu.Unlock()
	b := fb.rgba.Bounds()
	return b.Dx(), b.Dy()
}

// SetPixel paints one pixel with the decoder's RGB triple.
func (fb *Framebuffer) SetPixel(x, y int, r, g, b byte) {
	fb.mu.Lock()
	defer fb.mu.Unlock()
	bounds := fb.rgba.Bounds()
	if x < bounds.Min.X || x >= bounds.Max.X || y < bounds.Min.Y || y >= bounds.Max.Y {
		return
	}
	i := fb.rgba.PixOffset(x, y)
	fb.rgba.Pix[i] = r
	fb.rgba.Pix[i+1] = g
	fb.rgba.Pix[i+2] = b
	fb.rgba.Pix[i+3] = 255
	fb.markDirty(image.Rect(x, y, x+1, y+1))
}

// SetRow paints a horizontal run starting at (x, y).
func (fb *Framebuffer) SetRow(x, y int, rgb []byte) {
	fb.mu.Lock()
	defer fb.mu.Unlock()
	bounds := fb.rgba.Bounds()
	n := len(rgb) / 3
	if x < bounds.Min.X {
		skip := bounds.Min.X - x
		if skip >= n {
			return
		}
		rgb = rgb[skip*3:]
		n -= skip
		x = bounds.Min.X
	}
	if x+n > bounds.Max.X {
		n = bounds.Max.X - x
	}
	if y < bounds.Min.Y || y >= bounds.Max.Y || n <= 0 {
		return
	}
	i := fb.rgba.PixOffset(x, y)
	for k := 0; k < n; k++ {
		fb.rgba.Pix[i+k*4] = rgb[k*3]
		fb.rgba.Pix[i+k*4+1] = rgb[k*3+1]
		fb.rgba.Pix[i+k*4+2] = rgb[k*3+2]
		fb.rgba.Pix[i+k*4+3] = 255
	}
	fb.markDirty(image.Rect(x, y, x+n, y+1))
}

// BlitRGBA copies a decoded rectangle into the surface.
func (fb *Framebuffer) BlitRGBA(img *image.RGBA) {
	fb.mu.Lock()
	defer fb.mu.Unlock()
	bounds := fb.rgba.Bounds()
	r := img.Rect.Intersect(bounds)
	if r.Empty() {
		return
	}
	for y := r.Min.Y; y < r.Max.Y; y++ {
		srcY := y - img.Rect.Min.Y
		if srcY < 0 || srcY >= img.Rect.Dy() {
			continue
		}
		dstI := fb.rgba.PixOffset(r.Min.X, y)
		srcI := img.PixOffset(img.Rect.Min.X+r.Min.X-img.Rect.Min.X, srcY+img.Rect.Min.Y)
		n := r.Dx() * 4
		copy(fb.rgba.Pix[dstI:dstI+n], img.Pix[srcI:srcI+n])
	}
	fb.markDirty(r)
}

// markDirty accumulates damage; caller holds mu.
func (fb *Framebuffer) markDirty(r image.Rectangle) {
	if fb.dirty.Empty() {
		fb.dirty = r
	} else {
		fb.dirty = fb.dirty.Union(r)
	}
}

// Drain takes the accumulated damage rectangle, resetting it.
func (fb *Framebuffer) Drain() image.Rectangle {
	fb.mu.Lock()
	defer fb.mu.Unlock()
	r := fb.dirty
	fb.dirty = image.Rectangle{}
	return r
}

// Snapshot renders the full surface for an RFB client (full-frame update on
// connect / resolution change).
func (fb *Framebuffer) Snapshot() (*image.RGBA, int, int) {
	fb.mu.Lock()
	defer fb.mu.Unlock()
	out := image.NewRGBA(fb.rgba.Bounds())
	copy(out.Pix, fb.rgba.Pix)
	b := fb.rgba.Bounds()
	return out, b.Dx(), b.Dy()
}

// Modify runs fn with exclusive access to the raw pixel buffer (decoders
// batch-paint whole frames); fn returns the touched rectangle, which joins
// the accumulated damage. The returned geometry is the surface's before fn
// ran (decoders that Resize inside fn must return their own rect).
func (fb *Framebuffer) Modify(fn func(img *image.RGBA) image.Rectangle) {
	fb.mu.Lock()
	defer fb.mu.Unlock()
	r := fn(fb.rgba)
	if !r.Empty() {
		markDirtyRect(&fb.dirty, r)
	}
}

func markDirtyRect(cur *image.Rectangle, r image.Rectangle) {
	if cur.Empty() {
		*cur = r
	} else {
		*cur = cur.Union(r)
	}
}
