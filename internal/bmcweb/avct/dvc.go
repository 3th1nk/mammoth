package avct

import (
	"encoding/binary"
	"fmt"
	"image"
)

// DVC ("Dambrackas Video Compression") is the appliance's lossless update
// codec. The bitstream is a byte-oriented command sequence over a linear
// row-major pixel cursor (docs/compat/dell.md §视频通道):
//
//	1xxxxxxx            Make Pixel  — mode-specific colour payload follows
//	000nnnnn [ext...]   No Change   — advance the cursor by n
//	001nnnnn [ext...]   Copy Left   — fill n with the pixel left of the cursor
//	010nnnnn [ext...]   Copy Above  — copy n from the row above
//	011Cbbbb [cont...]  Make Series — two-colour bitmap, 4 then 7 pixels per byte
//
// Run lengths: 5 command bits extended by up to 4 continuation bytes that
// repeat the 3-bit opcode in their high bits (little-endian 5-bit groups).
// The 1-byte-per-pixel modes (DVC7 palette, DVC7 grey) add 2 to every run.
//
// Frame pacing: a BOF packet resets the cursor; EOF ends the frame. Commands
// span packet boundaries — trailing partial commands are stashed until the
// next packet (dropped at EOF). The packet payload layout:
//
//	0..3   unused
//	4..6   u16be height, u16be width
//	8      bit0 BOF (new frame, cursor reset), bit1 EOF
//	10..11 u16be checksum (unverified here; drift only degrades error checks)
//	12..   DVC bitstream
//
// Colour modes = the packet type byte:
//
//	0x81 DVC15      RGB555, 2 bytes per Make Pixel
//	0x82 DVC7       7-bit palette index, 1 byte
//	0x83 DVC7_GRAY  7-bit grey, 1 byte
//	0x8A DVC23      8/8/7-bit RGB, 3 bytes
//
// The appliance caps DVC resolution at 2000×2000 and ignores larger sizes.

const (
	dvcMode15    byte = 0x81
	dvcMode7     byte = 0x82
	dvcMode7Gray byte = 0x83
	dvcMode23    byte = 0x8A
)

func dvcExtraBytes(mode byte) int {
	switch mode {
	case dvcMode15:
		return 1
	case dvcMode23:
		return 2
	}
	return 0
}

func dvcRunBias(mode byte) int {
	if mode == dvcMode7 || mode == dvcMode7Gray {
		return 2
	}
	return 0
}

// dvcPalette builds the 128-entry 7-bit palette: five levels per channel
// (0/70/127/191/255) give 125 base colours plus three greys, mapped onto the
// wire index order.
var dvcPalette = buildDVCPalette()

func buildDVCPalette() [128]uint32 {
	levels := [5]uint32{0, 70, 127, 191, 255}
	var base [128]uint32
	for r := 0; r < 5; r++ {
		for g := 0; g < 5; g++ {
			for b := 0; b < 5; b++ {
				base[r*25+g*5+b] = levels[r]<<16 | levels[g]<<8 | levels[b]
			}
		}
	}
	base[125] = 0x5F5F5F
	base[126] = 0x9F9F9F
	base[127] = 0xDFDFDF
	// Wire index → base index permutation (reverse-engineered from the
	// viewer's palette builder).
	var perm = [128]byte{
		0, 25, 50, 75, 5, 30, 55, 80, 10, 35, 60, 85, 15, 40, 65, 90,
		1, 26, 51, 76, 6, 31, 56, 81, 11, 36, 61, 86, 16, 41, 66, 91,
		2, 27, 52, 77, 7, 32, 57, 82, 12, 37, 62, 87, 17, 42, 67, 92,
		3, 28, 53, 78, 8, 33, 58, 83, 13, 38, 63, 88, 18, 43, 68, 93,
		100, 101, 102, 103, 105, 106, 107, 108, 110, 111, 112, 113, 115, 116, 117, 118,
		20, 45, 70, 95, 21, 46, 71, 96, 22, 47, 72, 97, 23, 48, 73, 98,
		4, 29, 54, 79, 9, 34, 59, 84, 14, 39, 64, 89, 19, 44, 69, 94,
		120, 121, 122, 123, 104, 109, 114, 119, 24, 49, 74, 99, 124, 125, 126, 127,
	}
	var out [128]uint32
	for i := 0; i < 128; i++ {
		out[i] = base[perm[i]]
	}
	return out
}

// dvcMakePixel converts a Make Pixel command to 0x00RRGGBB.
func dvcMakePixel(mode, cmd, b1, b2 byte) uint32 {
	switch mode {
	case dvcMode15:
		// Wire is BGR555 (verified against the real R620: the ESXi amber
		// band decodes cyan with the RGB order, correct after the swap).
		v := uint32(cmd&0x7F)<<8 | uint32(b1)
		r := (v & 0x1F) << 3
		g := (v & 0x3E0) >> 2
		b := (v & 0x7C00) >> 7
		return r<<16 | g<<8 | b
	case dvcMode23:
		// Same channel order as DVC15 (blue in the high bits).
		v := uint32(cmd&0x7F)<<16 | uint32(b1)<<8 | uint32(b2)
		r := (v & 0x7F) << 1
		g := (v & 0x7F80) >> 7
		b := (v & 0x7F8000) >> 15
		return r<<16 | g<<8 | b
	case dvcMode7Gray:
		g := uint32(cmd&0x7F) << 1
		return g<<16 | g<<8 | g
	default:
		return dvcPalette[cmd&0x7F]
	}
}

// dvcDecoder holds the cross-packet decoder state of one video channel.
type dvcDecoder struct {
	fb       *Framebuffer
	started  bool // a BOF packet has been seen; pre-BOF packets are skipped
	cursor   int  // linear pixel index over the frame
	leftover []byte
}

// decode applies one video packet (already stripped of the 8-byte AVSP
// header) to the framebuffer.
func (d *dvcDecoder) decode(payload []byte, mode byte) error {
	if len(payload) < 12 {
		return fmt.Errorf("avct: dvc payload too short (%d)", len(payload))
	}
	width := int(binary.BigEndian.Uint16(payload[6:8]))
	height := int(binary.BigEndian.Uint16(payload[4:6]))
	flags := payload[8]
	bof := flags&1 != 0
	eof := flags&2 != 0

	w, h := d.fb.Size()
	if width > 0 && height > 0 && (width != w || height != h) {
		switch {
		case width > 2000 || height > 2000 || width > 4096 || height > 4096:
			sampleLog.Printf("avct: dvc ignoring oversized resolution %dx%d", width, height)
		default:
			sampleLog.Printf("avct: dvc resolution %dx%d -> %dx%d", w, h, width, height)
			d.fb.Resize(width, height)
			d.cursor = 0
			d.leftover = nil
		}
	}
	if bof {
		d.started = true
		d.cursor = 0
		d.leftover = nil
	}
	if !d.started {
		return nil
	}

	buf := d.leftover
	d.leftover = nil
	if len(buf) > 0 {
		buf = append(buf, payload[12:]...)
	} else {
		buf = payload[12:]
	}

	lo, hi := -1, -1
	d.fb.Modify(func(img *image.RGBA) image.Rectangle {
		w, h := img.Rect.Dx(), img.Rect.Dy()
		total := w * h
		set := func(i int, v uint32) {
			if i < 0 || i >= total {
				return
			}
			off := i * 4
			img.Pix[off] = byte(v >> 16)
			img.Pix[off+1] = byte(v >> 8)
			img.Pix[off+2] = byte(v)
			img.Pix[off+3] = 255
			if lo < 0 || i < lo {
				lo = i
			}
			if i >= hi {
				hi = i + 1
			}
		}
		pixelAt := func(i int) uint32 {
			if i < 0 || i >= total {
				return 0
			}
			off := i * 4
			return uint32(img.Pix[off])<<16 | uint32(img.Pix[off+1])<<8 | uint32(img.Pix[off+2])
		}
		pos := 0
		extra := dvcExtraBytes(mode)
		bias := dvcRunBias(mode)
		for pos < len(buf) {
			cmd := buf[pos]
			if cmd&0x80 != 0 { // Make Pixel
				if pos+1+extra > len(buf) {
					if !eof {
						d.leftover = append([]byte(nil), buf[pos:]...)
					}
					break
				}
				var b1, b2 byte
				if extra >= 1 {
					b1 = buf[pos+1]
				}
				if extra >= 2 {
					b2 = buf[pos+2]
				}
				set(d.cursor, dvcMakePixel(mode, cmd, b1, b2))
				d.cursor++
				pos += 1 + extra
				continue
			}
			op := cmd & 0xE0
			if op == 0x60 { // Make Series
				used, complete := dvcSeriesExtent(buf[pos:])
				if !complete && !eof {
					d.leftover = append([]byte(nil), buf[pos:]...)
					break
				}
				for _, v := range dvcSeriesPixels(pixelAt, d.cursor, buf[pos:pos+used]) {
					set(d.cursor, v)
					d.cursor++
				}
				pos += used
				continue
			}
			n, used, complete := dvcRunLength(buf[pos:])
			if !complete && !eof {
				d.leftover = append([]byte(nil), buf[pos:]...)
				break
			}
			n += bias
			switch op {
			case 0x00: // No Change
				d.cursor += n
			case 0x20: // Copy Left
				c := pixelAt(d.cursor - 1)
				for k := 0; k < n; k++ {
					set(d.cursor+k, c)
				}
				d.cursor += n
			case 0x40: // Copy Above (ignored on the first line: cursor stays)
				src := d.cursor - w
				if src >= 0 {
					for k := 0; k < n; k++ {
						set(d.cursor+k, pixelAt(src+k))
					}
					d.cursor += n
				}
			default:
				// Unknown opcode — bail out of the packet, resync at the next
				// BOF (the viewer treats the stream as frame-scoped too).
				pos = len(buf)
			}
			pos += used
			if d.cursor > total {
				d.cursor = total
			}
		}
		if lo < 0 || hi <= lo {
			return image.Rectangle{}
		}
		w2 := img.Rect.Dx()
		return image.Rect(0, lo/w2, w2, (hi-1)/w2+1)
	})
	return nil
}

// dvcRunLength parses a run-length command: 5 bits in the command byte,
// extended by up to 4 bytes carrying the same opcode and 5 more bits each.
func dvcRunLength(b []byte) (n, used int, complete bool) {
	op := b[0] & 0xE0
	n = int(b[0] & 0x1F)
	used = 1
	for k := 1; k < 5; k++ {
		if used >= len(b) {
			return n, used, false
		}
		if b[used]&0xE0 != op {
			return n, used, true
		}
		n |= int(b[used]&0x1F) << (5 * k)
		used++
	}
	return n, used, true
}

// dvcSeriesExtent measures a Make Series command (1 + continuation bytes
// while the high bit is set).
func dvcSeriesExtent(b []byte) (used int, complete bool) {
	used = 1
	if b[0]&0x10 == 0 {
		return used, true
	}
	for {
		if used >= len(b) {
			return used, false
		}
		used++
		if b[used-1]&0x80 == 0 {
			return used, true
		}
	}
}

// dvcSeriesPixels expands a Make Series command into colour values: colour
// A is the pixel before the cursor, colour B the most recent earlier pixel
// differing from A (A when none). The command byte carries 4 pixels (bits
// 3..0), each continuation byte 7 more (bits 6..0).
func dvcSeriesPixels(pixelAt func(int) uint32, cursor int, cmd []byte) []uint32 {
	var a uint32
	if cursor-1 >= 0 {
		a = pixelAt(cursor - 1)
	}
	b := a
	for k := cursor - 2; k >= 0; k-- {
		if c := pixelAt(k); c != a {
			b = c
			break
		}
	}
	pick := func(set bool) uint32 {
		if set {
			return b
		}
		return a
	}
	out := make([]uint32, 0, 4+7*(len(cmd)-1))
	c0 := cmd[0]
	out = append(out, pick(c0&8 != 0), pick(c0&4 != 0), pick(c0&2 != 0), pick(c0&1 != 0))
	for _, c := range cmd[1:] {
		out = append(out,
			pick(c&0x40 != 0), pick(c&0x20 != 0), pick(c&0x10 != 0), pick(c&0x08 != 0),
			pick(c&0x04 != 0), pick(c&0x02 != 0), pick(c&0x01 != 0))
	}
	return out
}
