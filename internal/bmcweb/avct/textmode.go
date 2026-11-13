package avct

import (
	"encoding/binary"
	"image"
)

// Text-mode video (BIOS/POST screens): the appliance sends the text buffer
// as {char, attr} pairs over an 8x16 glyph grid, a 16-colour palette and
// the glyph font (256 × 32 bytes, MSB-left rows) as separate packets.
//
// Packet 0x87 payload (after the AVSP header):
//
//	4..6   u16be buffer height, u16be width (pixels)
//	8      flags: bit3 blink, bit4 nine-dot graphics (9th column copies 8th)
//	9      underline scan row
//	10     rows, 11 columns
//	12..   rows*cols {char, attr} pairs; the last 4 bytes at EOF carry the
//	       cursor column/row/scan
//
// Attribute: high nibble background, low nibble foreground (bit 3 selects
// the alternate 128-glyph set when present). Glyph rows live at
// font[char*32 + row], bit 7 = leftmost pixel.

// TextMode keeps the font/palette state and renders text frames.
type TextMode struct {
	fb     *Framebuffer
	font   [256 * 32]byte
	pal    [16][3]byte
	blinks bool // blink enabled: bit 3 of the attribute alternates the bg
	nine   bool // 9-dot mode: column 9 replicates column 8
}

// NewTextMode binds a renderer to the framebuffer with VGA defaults.
func NewTextMode(fb *Framebuffer) *TextMode {
	t := &TextMode{fb: fb}
	t.resetPalette()
	return t
}

func (t *TextMode) resetPalette() {
	// VGA 16-colour default (the palette packet overrides it).
	def := [16][3]byte{
		{0, 0, 0}, {0, 0, 170}, {0, 170, 0}, {0, 170, 170},
		{170, 0, 0}, {170, 0, 170}, {170, 85, 0}, {170, 170, 170},
		{85, 85, 85}, {85, 85, 255}, {85, 255, 85}, {85, 255, 255},
		{255, 85, 85}, {255, 85, 255}, {255, 255, 85}, {255, 255, 255},
	}
	t.pal = def
}

// Font stores a font-table packet (0x89): 8192 bytes, 256 glyphs × 32 rows.
func (t *TextMode) Font(body []byte) {
	n := len(body)
	if n > len(t.font) {
		n = len(t.font)
	}
	copy(t.font[:n], body[:n])
}

// Palette stores a palette packet (0x88): u16be count + {r,g,b,pad} each.
func (t *TextMode) Palette(body []byte) {
	if len(body) < 2 {
		return
	}
	n := int(binary.BigEndian.Uint16(body))
	for i := 0; i < n && 2+i*4+3 <= len(body); i++ {
		o := 2 + i*4
		t.pal[i] = [3]byte{body[o], body[o+1], body[o+2]}
	}
}

// Video renders one text frame (0x87) into the framebuffer.
func (t *TextMode) Video(body []byte) {
	if len(body) < 12 {
		return
	}
	pixH := int(binary.BigEndian.Uint16(body[4:6]))
	pixW := int(binary.BigEndian.Uint16(body[6:8]))
	flags := body[8]
	underline := int(body[9])
	rows := int(body[10])
	cols := int(body[11])
	t.blinks = flags&8 != 0
	t.nine = flags&0x10 != 0

	cellW := 8
	if t.nine {
		cellW = 9
	}
	if rows <= 0 || cols <= 0 || rows > 200 || cols > 300 || pixW <= 0 || pixH <= 0 ||
		pixW > 4000 || pixH > 4000 || len(body) < 12+rows*cols*2 {
		return
	}
	if w, h := t.fb.Size(); w != pixW || h != pixH {
		t.fb.Resize(pixW, pixH)
	}

	cells := body[12 : 12+rows*cols*2]
	cursorCol, cursorRow, cursorScan := -1, -1, -1
	// The trailing 4 bytes at EOF carry the cursor position (column, row,
	// scan row, scan height); the EOF flag rides the packet header flags
	// which we do not have here — treat the trailer as present when the
	// payload is 4 bytes longer than the cells.
	if len(body) >= 12+rows*cols*2+4 {
		o := 12 + rows*cols*2
		cursorCol = int(body[o])
		cursorRow = int(body[o+1])
		cursorScan = int(body[o+2])
	}

	alt := t.blinks // bit-3 text with blink enabled uses the alternate set — approximated
	_ = alt
	for row := 0; row < rows; row++ {
		for col := 0; col < cols; col++ {
			o := (row*cols + col) * 2
			ch := cells[o]
			attr := cells[o+1]
			fg := attr & 0x0F
			bg := (attr >> 4) & 0x0F
			if t.blinks {
				fg = attr & 0x07
			}
			fgc := t.pal[fg&0x0F]
			bgc := t.pal[bg&0x0F]
			baseX, baseY := col*cellW, row*16
			glyph := t.font[int(ch)*32 : int(ch)*32+16]
			for gy := 0; gy < 16; gy++ {
				bits := glyph[gy]
				var rgb []byte
				if cellW == 9 {
					rgb = make([]byte, 0, 9*3)
					for gx := 0; gx < 8; gx++ {
						c := fgc
						if bits&(0x80>>uint(gx)) == 0 {
							c = bgc
						}
						rgb = append(rgb, c[0], c[1], c[2])
					}
					// Ninth column: replicate the eighth for line-drawing
					// glyphs 0xC0..0xDF, else background.
					if ch >= 0xC0 && ch <= 0xDF {
						rgb = append(rgb, rgb[7*3:8*3]...)
					} else {
						rgb = append(rgb, bgc[0], bgc[1], bgc[2])
					}
				} else {
					rgb = make([]byte, 0, 8*3)
					for gx := 0; gx < 8; gx++ {
						c := fgc
						if bits&(0x80>>uint(gx)) == 0 {
							c = bgc
						}
						rgb = append(rgb, c[0], c[1], c[2])
					}
				}
				// Underline row renders as foreground for non-space glyphs.
				if underline > 0 && gy == underline && ch != 0 && ch != ' ' {
					for k := 0; k < cellW; k++ {
						rgb[k*3], rgb[k*3+1], rgb[k*3+2] = fgc[0], fgc[1], fgc[2]
					}
				}
				t.fb.SetRow(baseX, baseY+gy, rgb)
			}
		}
	}

	// Hardware cursor: invert the scan rows of the current cell.
	if cursorCol >= 0 && cursorRow >= 0 && cursorScan >= 0 {
		t.invertCell(cursorCol, cursorRow, cursorScan)
	}
}

// invertCell flips the cursor scan row(s) of one cell in the framebuffer.
func (t *TextMode) invertCell(col, row, scan int) {
	cellW := 8
	if t.nine {
		cellW = 9
	}
	x0, y0 := col*cellW, row*16
	t.fb.Modify(func(img *image.RGBA) image.Rectangle {
		r := image.Rect(x0, y0, x0+cellW, y0+16).Intersect(img.Rect)
		if r.Empty() {
			return image.Rectangle{}
		}
		for y := r.Min.Y; y < r.Max.Y; y++ {
			for x := r.Min.X; x < r.Max.X; x++ {
				i := img.PixOffset(x, y)
				img.Pix[i] = 255 - img.Pix[i]
				img.Pix[i+1] = 255 - img.Pix[i+1]
				img.Pix[i+2] = 255 - img.Pix[i+2]
			}
		}
		return r
	})
}
