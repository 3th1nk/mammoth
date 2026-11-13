package avct

import (
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	"sync"
)

// ASpeed JPEG video decoder (packet type 0x86) — the lossy full-frame codec
// the iDRAC7/8's ASpeed GPU streams alongside the lossless DVC updates.
//
// Payload layout (after the AVSP header):
//
//	0      mode: bit0 = 0 → YUV 4:4:4 over 8x8 blocks, 1 → 4:2:0 over 16x16
//	       blocks; 2/3 carry a hardware cursor bitmap instead of video
//	1..3   cursor x (high 12 bits) / y (low 12 bits)
//	4..6   u16be height, u16be width
//	8      flags: bit0 = first fragment of a frame, bit4 = pad height to 16
//	9..11  hotspot x / hotspot y / cursor id
//	11     high nibble luminance quant table, low nibble chroma table
//	12..   compressed stream (little-endian u32 words, bits read MSB first)
//
// A frame's fragments accumulate until the NEXT first-fragment packet
// arrives — the picture is always exactly one frame behind the wire.
//
// Block grammar (4-bit codes): 0 JPEG, 4 low-quality JPEG, 5/6/7 vector-
// quantised 1/2/4-colour blocks, 8/12/13/14/15 their skip variants (20-bit
// header carrying an explicit block column/row), 9 frame end. A VQ header
// entry is 3 bits (load flag + 2-bit slot) optionally followed by 24 bits of
// Y,Cb,Cr; the palette persists within a frame.

const (
	aspeedJPEG       = 0
	aspeedJPEGSkip   = 8
	aspeedLowJPEG    = 4
	aspeedLowJPEGSkp = 12
	aspeedVQ1        = 5
	aspeedVQ2        = 6
	aspeedVQ4        = 7
	aspeedVQ1Skip    = 13
	aspeedVQ2Skip    = 14
	aspeedVQ4Skip    = 15
	aspeedFrameEnd   = 9
)

// ── bit reader ──────────────────────────────────────────────────────────────

// aspeedBits is the viewer's two-word sliding window: bits are consumed
// MSB-first out of little-endian u32 words.
type aspeedBits struct {
	words []uint32
	pos   int
	cur   uint32
	next  uint32
	avail int
}

func newASpeedBits(data []byte) (*aspeedBits, error) {
	n := len(data) / 4
	if n < 2 {
		return nil, fmt.Errorf("avct: aspeed frame too short (%d bytes)", len(data))
	}
	w := make([]uint32, n)
	for i := range w {
		w[i] = binary.LittleEndian.Uint32(data[4*i:])
	}
	return &aspeedBits{words: w, pos: 2, cur: w[0], next: w[1], avail: 32}, nil
}

// window is the 32-bit lookahead the block headers read from.
func (r *aspeedBits) window() uint32 { return r.cur }

func (r *aspeedBits) peek(n int) uint32 { return (r.cur >> uint(32-n)) & 0xFFFF }

func (r *aspeedBits) consume(n int) error {
	if n <= 0 || n > 31 {
		return fmt.Errorf("avct: aspeed consume(%d)", n)
	}
	if r.avail-n <= 0 {
		if r.pos >= len(r.words) {
			return errors.New("avct: aspeed bitstream truncated")
		}
		w := r.words[r.pos]
		r.pos++
		r.cur = r.cur<<uint(n) | (r.next|w>>uint(r.avail))>>uint(32-n)
		r.next = w << uint(n-r.avail)
		r.avail = 32 + r.avail - n
		return nil
	}
	r.cur = r.cur<<uint(n) | r.next>>uint(32-n)
	r.next <<= uint(n)
	r.avail -= n
	return nil
}

// extend reads an n-bit magnitude and sign-extends it per JPEG EXTEND.
func (r *aspeedBits) extend(n int) (int16, error) {
	if n < 1 || n > 15 {
		return 0, fmt.Errorf("avct: aspeed bad coefficient size %d", n)
	}
	v := int32(r.peek(n))
	if v&(1<<uint(n-1)) == 0 {
		v += int32(aspeedExtendOffset[n])
	}
	if err := r.consume(n); err != nil {
		return 0, err
	}
	return int16(v), nil
}

// ── Huffman ─────────────────────────────────────────────────────────────────

type aspeedHuff struct {
	minCode [17]int
	maxCode [17]int
	vals    [17][256]uint8
}

func newASpeedHuff(bits [17]uint8, vals []uint8) aspeedHuff {
	var t aspeedHuff
	k := 0
	for l := 1; l <= 16; l++ {
		for i := 0; i < int(bits[l]); i++ {
			if k < len(vals) && i < 256 {
				t.vals[l][i] = vals[k]
			}
			k++
		}
	}
	code := 0
	for l := 1; l <= 16; l++ {
		t.minCode[l] = code
		code += int(bits[l])
		t.maxCode[l] = code - 1
		code *= 2
		if bits[l] == 0 {
			t.minCode[l] = 0xFFFF
			t.maxCode[l] = 0
		}
	}
	return t
}

// decode reads one symbol: the first code length whose range contains the
// peeked bits wins.
func (t *aspeedHuff) decode(r *aspeedBits) (uint8, error) {
	for l := 1; l <= 16; l++ {
		c := int(r.peek(l))
		if c <= t.maxCode[l] && c >= t.minCode[l] {
			if err := r.consume(l); err != nil {
				return 0, err
			}
			idx := c - t.minCode[l]
			if idx >= 256 {
				return 0, errors.New("avct: aspeed invalid Huffman code")
			}
			return t.vals[l][idx], nil
		}
	}
	return 0, errors.New("avct: aspeed invalid Huffman code")
}

// ── constants ───────────────────────────────────────────────────────────────

// Huffman tables are JPEG Annex K.3 (the viewer hardcodes exactly these).
var (
	aspeedDCLuma = newASpeedHuff([17]uint8{0, 0, 1, 5, 1, 1, 1, 1, 1, 1, 0, 0, 0, 0, 0, 0, 0},
		[]uint8{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11})
	aspeedDCChroma = newASpeedHuff([17]uint8{0, 0, 3, 1, 1, 1, 1, 1, 1, 1, 1, 1, 0, 0, 0, 0, 0},
		[]uint8{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11})
	aspeedACLuma = newASpeedHuff([17]uint8{0, 0, 2, 1, 3, 3, 2, 4, 3, 5, 5, 4, 4, 0, 0, 1, 125}, aspeedACLumaVals)
	aspeedACChro = newASpeedHuff([17]uint8{0, 0, 2, 1, 2, 4, 4, 3, 4, 7, 5, 4, 4, 0, 1, 2, 119}, aspeedACChromaVals)
)

var aspeedACLumaVals = []uint8{
	1, 2, 3, 0, 4, 17, 5, 18, 33, 49, 65, 6, 19, 81, 97, 7,
	34, 113, 20, 50, 129, 145, 161, 8, 35, 66, 177, 193, 21, 82, 209, 240,
	36, 51, 98, 114, 130, 9, 10, 22, 23, 24, 25, 26, 37, 38, 39, 40,
	41, 42, 52, 53, 54, 55, 56, 57, 58, 67, 68, 69, 70, 71, 72, 73,
	74, 83, 84, 85, 86, 87, 88, 89, 90, 99, 100, 101, 102, 103, 104, 105,
	106, 115, 116, 117, 118, 119, 120, 121, 122, 131, 132, 133, 134, 135, 136, 137,
	138, 146, 147, 148, 149, 150, 151, 152, 153, 154, 162, 163, 164, 165, 166, 167,
	168, 169, 170, 178, 179, 180, 181, 182, 183, 184, 185, 186, 194, 195, 196, 197,
	198, 199, 200, 201, 202, 210, 211, 212, 213, 214, 215, 216, 217, 218, 225, 226,
	227, 228, 229, 230, 231, 232, 233, 234, 241, 242, 243, 244, 245, 246, 247, 248,
	249, 250,
}

var aspeedACChromaVals = []uint8{
	0, 1, 2, 3, 17, 4, 5, 33, 49, 6, 18, 65, 81, 7, 97, 113,
	19, 34, 50, 129, 8, 20, 66, 145, 161, 177, 193, 9, 35, 51, 82, 240,
	21, 98, 114, 209, 10, 22, 36, 52, 225, 37, 241, 23, 24, 25, 26, 38,
	39, 40, 41, 42, 53, 54, 55, 56, 57, 58, 67, 68, 69, 70, 71, 72,
	73, 74, 83, 84, 85, 86, 87, 88, 89, 90, 99, 100, 101, 102, 103, 104,
	105, 106, 115, 116, 117, 118, 119, 120, 121, 122, 130, 131, 132, 133, 134, 135,
	136, 137, 138, 146, 147, 148, 149, 150, 151, 152, 153, 154, 162, 163, 164, 165,
	166, 167, 168, 169, 170, 178, 179, 180, 181, 182, 183, 184, 185, 186, 194, 195,
	196, 197, 198, 199, 200, 201, 202, 210, 211, 212, 213, 214, 215, 216, 217, 218,
	226, 227, 228, 229, 230, 231, 232, 233, 234, 242, 243, 244, 245, 246, 247, 248,
	249, 250,
}

// Quantisation tables in natural order, selected by the packet header
// (0 coarsest .. 7 finest), pre-scaled by the fixed divisor 16.
var aspeedQuantLuma = [8][64]uint8{
	{20, 13, 12, 20, 30, 50, 63, 76, 15, 15, 17, 23, 32, 72, 75, 68, 17, 16, 20, 30, 50, 71, 86, 70, 17, 21, 27, 36, 63, 108, 100, 77, 22, 27, 46, 70, 85, 136, 128, 96, 30, 43, 68, 80, 101, 130, 141, 115, 61, 80, 97, 108, 128, 151, 150, 126, 90, 115, 118, 122, 140, 125, 128, 123},
	{17, 12, 10, 17, 26, 43, 55, 66, 13, 13, 15, 20, 28, 63, 65, 60, 15, 14, 17, 26, 43, 62, 75, 61, 15, 18, 24, 31, 55, 95, 87, 67, 19, 24, 40, 61, 74, 119, 112, 84, 26, 38, 60, 70, 88, 113, 123, 100, 53, 70, 85, 95, 112, 132, 131, 110, 78, 100, 103, 107, 122, 109, 112, 108},
	{14, 9, 9, 14, 21, 36, 46, 55, 10, 10, 12, 17, 23, 52, 54, 49, 12, 11, 14, 21, 36, 51, 62, 50, 12, 15, 19, 26, 46, 78, 72, 56, 16, 19, 33, 50, 61, 98, 93, 69, 21, 31, 49, 58, 73, 94, 102, 83, 44, 58, 70, 78, 93, 109, 108, 91, 65, 83, 86, 88, 101, 90, 93, 89},
	{11, 7, 7, 11, 17, 28, 36, 43, 8, 8, 10, 13, 18, 41, 43, 39, 10, 9, 11, 17, 28, 40, 49, 40, 10, 12, 15, 20, 36, 62, 57, 44, 12, 15, 26, 40, 48, 78, 74, 55, 17, 25, 39, 46, 58, 74, 81, 66, 35, 46, 56, 62, 74, 86, 86, 72, 51, 66, 68, 70, 80, 71, 74, 71},
	{9, 6, 5, 9, 13, 22, 28, 34, 6, 6, 7, 10, 14, 32, 33, 30, 7, 7, 9, 13, 22, 32, 38, 31, 7, 9, 12, 16, 28, 48, 45, 34, 10, 12, 20, 31, 38, 61, 57, 43, 13, 19, 30, 36, 45, 58, 63, 51, 27, 36, 43, 48, 57, 68, 67, 56, 40, 51, 53, 55, 63, 56, 57, 55},
	{6, 4, 3, 6, 9, 15, 19, 22, 4, 4, 5, 7, 9, 21, 22, 20, 5, 4, 6, 9, 15, 21, 25, 21, 5, 6, 8, 10, 19, 32, 30, 23, 6, 8, 13, 21, 25, 40, 38, 28, 9, 13, 20, 24, 30, 39, 42, 34, 18, 24, 29, 32, 38, 45, 45, 37, 27, 34, 35, 36, 42, 37, 38, 37},
	{3, 2, 1, 3, 4, 7, 9, 11, 2, 2, 2, 3, 4, 10, 11, 10, 2, 2, 3, 4, 7, 10, 12, 10, 2, 3, 4, 5, 9, 16, 15, 11, 3, 4, 6, 10, 12, 20, 19, 14, 4, 6, 10, 12, 15, 19, 21, 17, 9, 12, 14, 16, 19, 22, 22, 18, 13, 17, 17, 18, 21, 18, 19, 18},
	{2, 1, 1, 2, 3, 5, 6, 7, 1, 1, 1, 2, 3, 7, 7, 6, 1, 1, 2, 3, 5, 7, 8, 7, 1, 2, 2, 3, 6, 10, 10, 7, 2, 2, 4, 7, 8, 13, 12, 9, 3, 4, 6, 8, 10, 13, 14, 11, 6, 8, 9, 10, 12, 15, 15, 12, 9, 11, 11, 12, 14, 12, 12, 12},
}

var aspeedQuantChroma = [8][64]uint8{
	{31, 33, 45, 88, 185, 185, 185, 185, 33, 39, 48, 123, 185, 185, 185, 185, 45, 48, 105, 185, 185, 185, 185, 185, 88, 123, 185, 185, 185, 185, 185, 185, 185, 185, 185, 185, 185, 185, 185, 185, 185, 185, 185, 185, 185, 185, 185, 185, 185, 185, 185, 185, 185, 185, 185, 185, 185, 185, 185, 185, 185, 185, 185, 185},
	{27, 29, 39, 76, 160, 160, 160, 160, 29, 34, 42, 107, 160, 160, 160, 160, 39, 42, 91, 160, 160, 160, 160, 160, 76, 107, 160, 160, 160, 160, 160, 160, 160, 160, 160, 160, 160, 160, 160, 160, 160, 160, 160, 160, 160, 160, 160, 160, 160, 160, 160, 160, 160, 160, 160, 160, 160, 160, 160, 160, 160, 160, 160, 160},
	{22, 24, 32, 63, 133, 133, 133, 133, 24, 28, 34, 88, 133, 133, 133, 133, 32, 34, 75, 133, 133, 133, 133, 133, 63, 88, 133, 133, 133, 133, 133, 133, 133, 133, 133, 133, 133, 133, 133, 133, 133, 133, 133, 133, 133, 133, 133, 133, 133, 133, 133, 133, 133, 133, 133, 133, 133, 133, 133, 133, 133, 133, 133, 133},
	{18, 19, 26, 51, 108, 108, 108, 108, 19, 22, 28, 72, 108, 108, 108, 108, 26, 28, 61, 108, 108, 108, 108, 108, 51, 72, 108, 108, 108, 108, 108, 108, 108, 108, 108, 108, 108, 108, 108, 108, 108, 108, 108, 108, 108, 108, 108, 108, 108, 108, 108, 108, 108, 108, 108, 108, 108, 108, 108, 108, 108, 108, 108, 108},
	{13, 14, 19, 38, 80, 80, 80, 80, 14, 17, 21, 53, 80, 80, 80, 80, 19, 21, 45, 80, 80, 80, 80, 80, 38, 53, 80, 80, 80, 80, 80, 80, 80, 80, 80, 80, 80, 80, 80, 80, 80, 80, 80, 80, 80, 80, 80, 80, 80, 80, 80, 80, 80, 80, 80, 80, 80, 80, 80, 80, 80, 80, 80, 80},
	{9, 10, 13, 26, 55, 55, 55, 55, 10, 11, 14, 37, 55, 55, 55, 55, 13, 14, 31, 55, 55, 55, 55, 55, 26, 37, 55, 55, 55, 55, 55, 55, 55, 55, 55, 55, 55, 55, 55, 55, 55, 55, 55, 55, 55, 55, 55, 55, 55, 55, 55, 55, 55, 55, 55, 55, 55, 55, 55, 55, 55, 55, 55, 55},
	{4, 5, 6, 13, 27, 27, 27, 27, 5, 5, 7, 18, 27, 27, 27, 27, 6, 7, 15, 27, 27, 27, 27, 27, 13, 18, 27, 27, 27, 27, 27, 27, 27, 27, 27, 27, 27, 27, 27, 27, 27, 27, 27, 27, 27, 27, 27, 27, 27, 27, 27, 27, 27, 27, 27, 27, 27, 27, 27, 27, 27, 27, 27, 27},
	{3, 3, 4, 8, 18, 18, 18, 18, 3, 3, 4, 12, 18, 18, 18, 18, 4, 4, 10, 18, 18, 18, 18, 18, 8, 12, 18, 18, 18, 18, 18, 18, 18, 18, 18, 18, 18, 18, 18, 18, 18, 18, 18, 18, 18, 18, 18, 18, 18, 18, 18, 18, 18, 18, 18, 18, 18, 18, 18, 18, 18, 18, 18, 18},
}

var aspeedZigzag = [64]uint8{
	0, 1, 8, 16, 9, 2, 3, 10,
	17, 24, 32, 25, 18, 11, 4, 5,
	12, 19, 26, 33, 40, 48, 41, 34,
	27, 20, 13, 6, 7, 14, 21, 28,
	35, 42, 49, 56, 57, 50, 43, 36,
	29, 22, 15, 23, 30, 37, 44, 51,
	58, 59, 52, 45, 38, 31, 39, 46,
	53, 60, 61, 54, 47, 55, 62, 63,
}

var aspeedExtendOffset = [16]int16{0, -1, -3, -7, -15, -31, -63, -127, -255, -511, -1023, -2047, -4095, -8191, -16383, -32767}

var aspeedAANScale = [8]float32{1.0, 1.3870399, 1.306563, 1.1758755, 1.0, 0.78569496, 0.5411961, 0.27589938}

// Colour conversion + clamp tables (fixed point, built once).
var (
	aspeedOnce     sync.Once
	aspeedCrToR    [256]int
	aspeedCbToB    [256]int
	aspeedCrToG    [256]int
	aspeedCbToG    [256]int
	aspeedYScale   [256]int
	aspeedClamp    [1408]uint8
	aspeedQuantTab [4][64]float32 // per-decoder base tables are derived on demand
)

func aspeedInitTables() {
	aspeedOnce.Do(func() {
		fix := func(d float64) int { return int(d*65536 + 0.5) }
		for i, n := 0, -128; i < 256; i, n = i+1, n+1 {
			aspeedCrToR[i] = (fix(1.597656)*n + 32768) >> 16
			aspeedCbToB[i] = (fix(2.015625)*n + 32768) >> 16
			aspeedCrToG[i] = -fix(0.8125) * n
			aspeedCbToG[i] = -fix(0.390625)*n + 32768
		}
		for i, n := 0, -16; i < 256; i, n = i+1, n+1 {
			aspeedYScale[i] = (fix(1.164)*n + 32768) >> 16
		}
		for i := 0; i < 256; i++ {
			aspeedClamp[i] = 0
			aspeedClamp[256+i] = uint8(i)
		}
		for i := 256; i < 640; i++ {
			aspeedClamp[256+i] = 255
		}
	})
}

func aspeedClampAt(i int) uint32 {
	if i < 0 || i >= len(aspeedClamp) {
		return 0
	}
	return uint32(aspeedClamp[i])
}

// aspeedYUV converts one Y,Cb,Cr triple to 0xFFRRGGBB.
func aspeedYUV(y, cb, cr uint8) uint32 {
	iy := aspeedYScale[y]
	b := aspeedClampAt(256 + iy + aspeedCbToB[cb])
	g := aspeedClampAt(256 + iy + ((aspeedCbToG[cb] + aspeedCrToG[cr]) >> 16))
	r := aspeedClampAt(256 + iy + aspeedCrToR[cr])
	return 0xFF000000 | r<<16 | g<<8 | b
}

// buildQuant dequant-scales a wire table: q*16/16==q per entry in natural
// order, multiplied by the AAN row/column scale factors.
func aspeedBuildQuant(src [64]uint8, dst *[64]float32) {
	for i := 0; i < 64; i++ {
		dst[aspeedZigzag[i]] = float32(src[i])
	}
	n := 0
	for row := 0; row < 8; row++ {
		for col := 0; col < 8; col++ {
			dst[n] = dst[n] * (aspeedAANScale[row] * aspeedAANScale[col])
			n++
		}
	}
}

// ── decoder ─────────────────────────────────────────────────────────────────

type aspeedDecoder struct {
	fb *Framebuffer

	pending []byte // accumulated fragments of the frame awaiting decode
	hasPend bool

	configured bool
	mode420    bool
	quant      [4][64]float32 // Y, UV, low-quality Y, low-quality UV

	blockX, blockY        int
	predY, predCb, predCr int16
	vqColours             [4]uint32
	vqIndex               [4]int
	vqBits                int
	width, height         int
	padHeight             int

	coef   [64]int16
	ws     [64]int32
	planes [6][64]uint8 // 4:2:0 → 4×Y + Cb + Cr; 4:4:4 → Y, Cb, Cr
	pix    [256]uint32
}

func newASpeedDecoder(fb *Framebuffer) *aspeedDecoder {
	aspeedInitTables()
	return &aspeedDecoder{fb: fb}
}

// handle processes one 0x86 payload: first-fragment packets decode the
// previous accumulated frame and open a new one; the rest are appended.
func (d *aspeedDecoder) handle(payload []byte) error {
	if len(payload) < 12 {
		return fmt.Errorf("avct: aspeed payload too short (%d)", len(payload))
	}
	first := payload[8]&1 != 0
	data := payload[12:]
	if !first {
		if d.hasPend {
			d.pending = append(d.pending, data...)
		}
		return nil
	}
	prev := d.pending
	prevHas := d.hasPend
	d.pending = append([]byte(nil), data...)
	d.hasPend = true
	if prevHas {
		return d.decodeFrame(prev)
	}
	return nil
}

// decodeFrame decodes one complete frame payload (12-byte header + data).
func (d *aspeedDecoder) decodeFrame(p []byte) error {
	if len(p) < 12 {
		return fmt.Errorf("avct: aspeed frame too short (%d)", len(p))
	}
	mode := int(p[0])
	if mode == 2 || mode == 3 {
		return nil // hardware cursor bitmap: kept out of the framebuffer
	}
	mode420 := mode&1 == 1
	yTable := int(p[11] >> 4)
	uvTable := int(p[11] & 0xF)
	if yTable > 7 || uvTable > 7 {
		return fmt.Errorf("avct: aspeed unsupported quant selector Y=%d UV=%d", yTable, uvTable)
	}
	if !d.configured || d.mode420 != mode420 {
		d.configured = true
		d.mode420 = mode420
		aspeedBuildQuant(aspeedQuantLuma[yTable], &d.quant[0])
		aspeedBuildQuant(aspeedQuantChroma[uvTable], &d.quant[1])
		aspeedBuildQuant(aspeedQuantLuma[0], &d.quant[2])
		aspeedBuildQuant(aspeedQuantChroma[0], &d.quant[3])
	}

	width := int(binary.BigEndian.Uint16(p[6:8]))
	height := int(binary.BigEndian.Uint16(p[4:6]))
	if width <= 0 || height <= 0 || width > 4096 || height > 4096 {
		return fmt.Errorf("avct: aspeed bad frame size %dx%d", width, height)
	}
	d.width, d.height = width, height
	d.padHeight = height
	if height == 600 && mode420 {
		d.padHeight = 608
	} else if p[8]&0x10 != 0 && height%16 != 0 {
		d.padHeight = height/16*16 + 16
	}
	if w, h := d.fb.Size(); (w != width || h != height) && width <= 2000 && height <= 2000 {
		d.fb.Resize(width, height)
	}

	r, err := newASpeedBits(p[12:])
	if err != nil {
		return err
	}
	d.blockX, d.blockY = 0, 0
	d.predY, d.predCb, d.predCr = 0, 0, 0
	d.vqIndex = [4]int{0, 1, 2, 3}
	d.vqColours = [4]uint32{0x008080, 0xFF8080, 0x808080, 0xC08080}
	d.vqBits = 0

	d.fb.Modify(func(img *image.RGBA) image.Rectangle {
		return d.decodeBlocks(img, r)
	})
	return nil
}

// decodeBlocks runs the block loop until the frame-end code.
func (d *aspeedDecoder) decodeBlocks(img *image.RGBA, r *aspeedBits) image.Rectangle {
	var dirty image.Rectangle
	bs := 8
	if d.mode420 {
		bs = 16
	}
	for {
		code := int(r.window() >> 28)
		switch code {
		case aspeedFrameEnd:
			return dirty
		case aspeedJPEG, aspeedJPEGSkip, aspeedLowJPEG, aspeedLowJPEGSkp:
			if d.readPosition(r, code&8 != 0) != nil {
				return dirty
			}
			tbl := 0
			if code&4 != 0 {
				tbl = 2
			}
			if err := d.jpegBlock(r, tbl); err != nil {
				sampleLog.Printf("avct: aspeed jpeg block (%d,%d): %v", d.blockX, d.blockY, err)
				return dirty
			}
		case aspeedVQ1, aspeedVQ2, aspeedVQ4, aspeedVQ1Skip, aspeedVQ2Skip, aspeedVQ4Skip:
			if d.readPosition(r, code&8 != 0) != nil {
				return dirty
			}
			d.vqBits = code&3 - 1
			for i := 0; i < 1<<uint(d.vqBits); i++ {
				if err := d.readVQColour(r, i); err != nil {
					return dirty
				}
			}
			if err := d.vqBlock(r); err != nil {
				return dirty
			}
		default:
			sampleLog.Printf("avct: aspeed unknown block code %d at (%d,%d)", code, d.blockX, d.blockY)
			return dirty
		}
		dirty = dirty.Union(d.blit(img, bs))
		d.advance(bs)
	}
}

// readPosition consumes the 4-bit code (skip variants carry 12 more bits of
// block column/row).
func (d *aspeedDecoder) readPosition(r *aspeedBits, skip bool) error {
	if !skip {
		return r.consume(4)
	}
	w := r.window()
	d.blockX = int(w >> 20 & 0xFF)
	d.blockY = int(w >> 12 & 0xFF)
	return r.consume(20)
}

// readVQColour loads one palette slot: 3-bit flag+slot, optionally 24-bit
// YCbCr colour.
func (d *aspeedDecoder) readVQColour(r *aspeedBits, i int) error {
	w := r.window()
	d.vqIndex[i] = int(w >> 29 & 3)
	if w>>31&1 == 0 {
		return r.consume(3)
	}
	d.vqColours[d.vqIndex[i]] = w >> 5 & 0xFFFFFF
	return r.consume(27)
}

// vqBlock paints 64 pixels from the palette per the index bits.
func (d *aspeedDecoder) vqBlock(r *aspeedBits) error {
	for k := 0; k < 64; k++ {
		var idx int
		if d.vqBits > 0 {
			idx = d.vqIndex[r.peek(d.vqBits)]
			if err := r.consume(d.vqBits); err != nil {
				return err
			}
		} else {
			idx = d.vqIndex[0]
		}
		c := d.vqColours[idx]
		d.planes[0][k] = uint8(c >> 16)
		d.planes[1][k] = uint8(c >> 8)
		d.planes[2][k] = uint8(c)
	}
	if d.mode420 {
		return nil // the viewer cannot draw VQ in 4:2:0; reproduce the skip
	}
	d.convert444()
	return nil
}

// jpegBlock Huffman-decodes + IDCTs one macroblock of Y (1 or 4 blocks),
// Cb, Cr.
func (d *aspeedDecoder) jpegBlock(r *aspeedBits, tbl int) error {
	ny := 1
	if d.mode420 {
		ny = 4
	}
	for i := 0; i < ny; i++ {
		if err := d.coefBlock(r, &aspeedDCLuma, &aspeedACLuma, &d.predY); err != nil {
			return err
		}
		d.idct(&d.quant[tbl], &d.planes[i])
	}
	if err := d.coefBlock(r, &aspeedDCChroma, &aspeedACChro, &d.predCb); err != nil {
		return err
	}
	d.idct(&d.quant[tbl+1], &d.planes[ny])
	if err := d.coefBlock(r, &aspeedDCChroma, &aspeedACChro, &d.predCr); err != nil {
		return err
	}
	d.idct(&d.quant[tbl+1], &d.planes[ny+1])
	if d.mode420 {
		d.convert420()
	} else {
		d.convert444()
	}
	return nil
}

// coefBlock decodes one 8x8 coefficient block (zig-zag → natural order),
// updating the DC predictor.
func (d *aspeedDecoder) coefBlock(r *aspeedBits, dc, ac *aspeedHuff, pred *int16) error {
	var zz [64]int16
	sym, err := dc.decode(r)
	if err != nil {
		return err
	}
	if sym == 0 {
		zz[0] = *pred
	} else {
		diff, err := r.extend(int(sym))
		if err != nil {
			return err
		}
		zz[0] = *pred + diff
		*pred = zz[0]
	}
	k := 1
	for k <= 63 {
		sym, err := ac.decode(r)
		if err != nil {
			return err
		}
		size, run := int(sym&0xF), int(sym>>4)
		if size == 0 {
			if run == 0 {
				break // EOB
			}
			if run == 15 {
				k += 16 // ZRL
			}
			continue
		}
		k += run
		if k >= 64 {
			return fmt.Errorf("avct: aspeed AC index %d out of range", k)
		}
		v, err := r.extend(size)
		if err != nil {
			return err
		}
		zz[k] = v
		k++
	}
	for i := 0; i < 64; i++ {
		d.coef[i] = zz[aspeedZigzag[i]]
	}
	return nil
}

// mul is the 8-fraction-bit fixed-point multiply of the viewer's IDCT.
func aspeedMul(a, b int32) int32 { return a * b >> 8 }

func aspeedIDCT1D(x0, x1, x2, x3, x4, x5, x6, x7 int32, o *[8]int32) {
	n14 := x0 + x4
	n13 := x0 - x4
	n12 := x2 + x6
	n11 := aspeedMul(x2-x6, 362) - n12
	n18 := n14 + n12
	n15 := n14 - n12
	n17 := n13 + n11
	n16 := n13 - n11
	n6 := x5 + x3
	n5 := x5 - x3
	n4 := x1 + x7
	n3 := x1 - x7
	n7 := n4 + n6
	n13 = aspeedMul(n4-n6, 362)
	n2 := aspeedMul(n5+n3, 473)
	n14 = aspeedMul(n3, 277) - n2
	n11 = aspeedMul(n5, -669) + n2
	n8 := n11 - n7
	n9 := n13 - n8
	n10 := n14 + n9
	o[0] = n18 + n7
	o[7] = n18 - n7
	o[1] = n17 + n8
	o[6] = n17 - n8
	o[2] = n16 + n9
	o[5] = n16 - n9
	o[4] = n15 + n10
	o[3] = n15 - n10
}

// idct dequantises d.coef with q and writes clamped 8x8 samples to plane.
func (d *aspeedDecoder) idct(q *[64]float32, plane *[64]uint8) {
	var ws [64]int32
	for c := 0; c < 8; c++ {
		if d.coef[c+8]|d.coef[c+16]|d.coef[c+24]|d.coef[c+32]|d.coef[c+40]|d.coef[c+48]|d.coef[c+56] == 0 {
			dc := int32(float32(d.coef[c]) * q[c])
			for i := 0; i < 8; i++ {
				ws[c+8*i] = dc
			}
			continue
		}
		x0 := int32(float32(d.coef[c]) * q[c])
		x2 := int32(float32(d.coef[c+16]) * q[c+16])
		x4 := int32(float32(d.coef[c+32]) * q[c+32])
		x6 := int32(float32(d.coef[c+48]) * q[c+48])
		x1 := int32(float32(d.coef[c+8]) * q[c+8])
		x3 := int32(float32(d.coef[c+24]) * q[c+24])
		x5 := int32(float32(d.coef[c+40]) * q[c+40])
		x7 := int32(float32(d.coef[c+56]) * q[c+56])
		var o [8]int32
		aspeedIDCT1D(x0, x1, x2, x3, x4, x5, x6, x7, &o)
		for i := 0; i < 8; i++ {
			ws[c+8*i] = o[i]
		}
	}
	for r := 0; r < 8; r++ {
		base := r * 8
		var o [8]int32
		aspeedIDCT1D(ws[base], ws[base+1], ws[base+2], ws[base+3], ws[base+4], ws[base+5], ws[base+6], ws[base+7], &o)
		for i := 0; i < 8; i++ {
			plane[base+i] = aspeedClamp[(384+int(o[i]>>3))&0x3FF]
		}
	}
}

// convert444 fills the pixel cache from planes Y, Cb, Cr.
func (d *aspeedDecoder) convert444() {
	for m := 0; m < 64; m++ {
		d.pix[m] = aspeedYUV(d.planes[0][m], d.planes[1][m], d.planes[2][m])
	}
}

// convert420 fills the pixel cache for a 16x16 macroblock (4 Y blocks + 1
// subsampled Cb/Cr each).
func (d *aspeedDecoder) convert420() {
	for row := 0; row < 16; row++ {
		for col := 0; col < 16; col++ {
			y := d.planes[(row/8)*2+col/8][(row%8)*8+col%8]
			ci := ((row >> 1) << 3) + (col >> 1)
			d.pix[row*16+col] = aspeedYUV(y, d.planes[4][ci], d.planes[5][ci])
		}
	}
}

// blit copies the current block into img, clipped.
func (d *aspeedDecoder) blit(img *image.RGBA, bs int) image.Rectangle {
	x0, y0 := d.blockX*bs, d.blockY*bs
	rect := image.Rect(x0, y0, x0+bs, y0+bs).Intersect(img.Rect)
	if rect.Empty() {
		return image.Rectangle{}
	}
	W := img.Rect.Dx()
	for y := rect.Min.Y; y < rect.Max.Y; y++ {
		src := (y - y0) * bs
		dst := y*W + rect.Min.X
		for x := rect.Min.X; x < rect.Max.X; x++ {
			v := d.pix[src+x-x0]
			i := (dst + x - rect.Min.X) * 4
			img.Pix[i] = byte(v >> 16)
			img.Pix[i+1] = byte(v >> 8)
			img.Pix[i+2] = byte(v)
			img.Pix[i+3] = 255
		}
	}
	return rect
}

// advance walks the block grid row-major over width/bs columns and
// padHeight/bs rows.
func (d *aspeedDecoder) advance(bs int) {
	d.blockX++
	if d.blockX >= d.width/bs {
		d.blockY++
		if d.blockY >= d.padHeight/bs {
			d.blockY = 0
		}
		d.blockX = 0
	}
}

// putARGB writes one 0xFFRRGGBB pixel (kept for symmetry with the DVC blit).
func putARGB(pix []uint8, i int, v uint32) {
	pix[i*4] = byte(v >> 16)
	pix[i*4+1] = byte(v >> 8)
	pix[i*4+2] = byte(v)
	pix[i*4+3] = 255
}
