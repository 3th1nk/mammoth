package ipmi

import (
	"bytes"
	"errors"
	"fmt"
	"testing"

	goipmi "github.com/vmware/goipmi"

	"github.com/3th1nk/mammoth/internal/bmc"
)

// fruField is one encoded TLV entry for the synthetic area builder.
type fruField struct {
	typ byte // FRU field type code: 0 binary, 2 six-bit, 3 eight-bit ASCII
	val []byte
}

func ascii(s string) fruField  { return fruField{typ: 3, val: []byte(s)} }
func packed(s string) fruField { return fruField{typ: 2, val: pack6bit(s)} }

// buildArea assembles one FRU area: version + length (+ mfg datetime when
// headerSkip is the board variant), then the given TLVs, end marker,
// zero padding and an ignored checksum byte — sized to whole 8-byte blocks.
func buildArea(fields []fruField, headerSkip int) []byte {
	body := []byte{0x01, 0x00}
	for i := 2; i < headerSkip; i++ {
		body = append(body, 0x00) // mfg datetime filler on board areas
	}
	for _, f := range fields {
		body = append(body, f.typ<<6|byte(len(f.val)))
		body = append(body, f.val...)
	}
	body = append(body, 0xC1) // end of fields
	for len(body)%8 != 7 {
		body = append(body, 0x00)
	}
	body = append(body, 0x00) // checksum (parseFRU is deliberately lenient)
	body[1] = byte(len(body) / 8)
	return body
}

// buildFRU assembles a whole FRU inventory area: common header + board +
// product areas, offsets in 8-byte blocks per the storage definition v1.3.
func buildFRU(board, product []fruField) []byte {
	boardArea := buildArea(board, 5)
	productArea := buildArea(product, 2)
	hdr := make([]byte, 8)
	hdr[0] = 0x01
	hdr[3] = byte(8 / 8)
	hdr[4] = byte((8 + len(boardArea)) / 8)
	var sum byte
	for _, b := range hdr[:6] {
		sum += b
	}
	hdr[6] = -sum
	return append(append(hdr, boardArea...), productArea...)
}

// pack6bit is the encoder counterpart of decode6bitASCII (chars biased by
// 0x20, packed six bits at a time, four chars into three bytes).
func pack6bit(s string) []byte {
	codes := []byte(s)
	for i := range codes {
		codes[i] -= 0x20
	}
	var out []byte
	for i := 0; i < len(codes); i += 4 {
		c := codes[i:min(i+4, len(codes))]
		for len(c) < 4 {
			c = append(c, 0)
		}
		out = append(out,
			c[0]&0x3f|(c[1]&0x03)<<6,
			(c[1]&0x3c)>>2|(c[2]&0x0f)<<4,
			(c[2]&0x30)>>4|(c[3]&0x3f)<<2,
		)
	}
	return out
}

// r710FRU mirrors the real machine this driver was written against: board
// "PowerEdge R710", product label "bluefish", service tag 23Q683X.
func r710FRU() []byte {
	return buildFRU(
		[]fruField{ascii("DELL"), ascii("PowerEdge R710"), ascii("CN137401A8004F"), ascii("0XDX06A12")},
		[]fruField{ascii("DELL"), ascii("bluefish"), ascii("0XDX06A12"), ascii("02"), ascii("23Q683X")},
	)
}

func TestParseFRUR710(t *testing.T) {
	fru, err := parseFRU(r710FRU())
	if err != nil {
		t.Fatalf("parseFRU: %v", err)
	}
	if fru.BoardProduct != "PowerEdge R710" {
		t.Errorf("BoardProduct = %q", fru.BoardProduct)
	}
	if fru.BoardSerial != "CN137401A8004F" {
		t.Errorf("BoardSerial = %q", fru.BoardSerial)
	}
	if fru.ProductName != "bluefish" {
		t.Errorf("ProductName = %q", fru.ProductName)
	}
	if fru.ProductSerial != "23Q683X" {
		t.Errorf("ProductSerial = %q", fru.ProductSerial)
	}
}

func TestParseFRUFieldTypes(t *testing.T) {
	area := buildFRU(
		[]fruField{ascii("DELL"), ascii("R640"), packed("DELL")},
		[]fruField{ascii("DELL"), ascii("host"), ascii("P1"), ascii("v1"), {typ: 0, val: []byte{0xde, 0xad}}},
	)
	fru, err := parseFRU(area)
	if err != nil {
		t.Fatalf("parseFRU: %v", err)
	}
	if fru.BoardProduct != "R640" {
		t.Errorf("BoardProduct = %q", fru.BoardProduct)
	}
	if fru.BoardSerial != "DELL" {
		t.Errorf("6-bit packed serial = %q, want DELL", fru.BoardSerial)
	}
	// Binary fields render as hex; an empty product serial keeps the board
	// fallback path intact.
	if fru.ProductSerial != "dead" {
		t.Errorf("binary product serial = %q, want hex dead", fru.ProductSerial)
	}
}

func TestParseFRUEdges(t *testing.T) {
	if _, err := parseFRU(r710FRU()[:7]); err == nil {
		t.Error("short area must error")
	}
	bad := r710FRU()
	bad[0] = 0x02
	if _, err := parseFRU(bad); err == nil {
		t.Error("unknown format version must error")
	}
	// No board/product pointers: empty identity, no error (lenient parse).
	headless := make([]byte, 8)
	headless[0] = 0x01
	fru, err := parseFRU(headless)
	if err != nil {
		t.Fatalf("header-only FRU: %v", err)
	}
	if fru != (fruInfo{}) {
		t.Errorf("header-only FRU = %+v, want zero", fru)
	}
}

func TestFRUReadResponseCountGuard(t *testing.T) {
	var r fruReadResponse
	if err := r.UnmarshalBinary([]byte{0x00, 0x05, 0xaa}); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if r.Count != 1 || !bytes.Equal(r.Data, []byte{0xaa}) {
		t.Errorf("count over-report not clamped: count=%d data=%v", r.Count, r.Data)
	}
}

// chunkReader answers byte-addressed Read FRU Data requests off a slice,
// the way a real BMC would.
func chunkReader(area []byte) func(offset, count uint16) ([]byte, error) {
	return func(offset, count uint16) ([]byte, error) {
		if int(offset)+int(count) > len(area) {
			return nil, fmt.Errorf("read past area: offset %d count %d size %d", offset, count, len(area))
		}
		return area[offset : offset+count], nil
	}
}

func TestReadFRUChunks(t *testing.T) {
	area := make([]byte, 100)
	for i := range area {
		area[i] = byte(i)
	}
	got, err := readFRUChunks(chunkReader(area), len(area))
	if err != nil || !bytes.Equal(got, area) {
		t.Errorf("roundtrip: err=%v got %d bytes, want %d", err, len(got), len(area))
	}
	// A partial last chunk must not be dropped or padded.
	got, err = readFRUChunks(chunkReader(area), 97)
	if err != nil || len(got) != 97 {
		t.Errorf("partial tail: err=%v got %d bytes, want 97", err, len(got))
	}
}

func TestReadFRUChunksAdaptiveChunk(t *testing.T) {
	area := r710FRU()
	// An iDRAC6-flavored read: counts above 16 answer completion code 0xCA
	// (cannot return that many bytes), smaller counts succeed. The loop must
	// halve its chunk and still reassemble the whole area.
	floor := 16
	read := func(offset, count uint16) ([]byte, error) {
		if int(count) > floor {
			return nil, goipmi.CompletionCode(0xca)
		}
		return chunkReader(area)(offset, count)
	}
	got, err := readFRUChunks(read, len(area))
	if err != nil || !bytes.Equal(got, area) {
		t.Fatalf("adaptive roundtrip: err=%v got %d bytes, want %d", err, len(got), len(area))
	}
	// At the floor a completion-code rejection is fatal, not a hang.
	// count > 3 rejects everything, including the minimum chunk of 4.
	floor = 3
	if _, err := readFRUChunks(read, len(area)); err == nil {
		t.Error("rejection at chunk floor must be fatal")
	}
}

func TestReadFRUChunksAdaptiveToolText(t *testing.T) {
	area := r710FRU()
	// The lanplus transport shells out to ipmitool: rejections arrive as
	// stderr text carrying "rsp=0x.." instead of a typed completion code.
	toolText := func(count uint16) error {
		return fmt.Errorf("run /usr/bin/ipmitool ipmitool -H bmc -I lanplus raw 0x0a 0x11 0x00 0x00 0x00 %#x: Unable to send RAW command (channel=0x0 netfn=0xa lun=0x0 cmd=0x11 rsp=0xca): Cannot return number of requested data bytes\n (exit status 1)", count)
	}
	read := func(offset, count uint16) ([]byte, error) {
		if count > 16 {
			return nil, toolText(count)
		}
		return chunkReader(area)(offset, count)
	}
	got, err := readFRUChunks(read, len(area))
	if err != nil || !bytes.Equal(got, area) {
		t.Fatalf("tool-text adaptive roundtrip: err=%v got %d bytes, want %d", err, len(got), len(area))
	}
	// Non-completion-code failures must stay fatal without any retry.
	read = func(offset, count uint16) ([]byte, error) {
		return nil, fmt.Errorf("unable to establish IPMI v2 / RMCP+ session")
	}
	if _, err := readFRUChunks(read, len(area)); err == nil {
		t.Error("session failure must be fatal")
	}
}

func TestReadFRUChunksGuards(t *testing.T) {
	if _, err := readFRUChunks(chunkReader(make([]byte, 16)), 0); err == nil {
		t.Error("zero size must error")
	}
	if _, err := readFRUChunks(chunkReader(make([]byte, 16)), maxFRUBytes+1); err == nil {
		t.Error("implausible size must error")
	}
	// A firmware answering with zero-length chunks must not loop forever.
	_, err := readFRUChunks(func(uint16, uint16) ([]byte, error) { return nil, nil }, 64)
	if err == nil {
		t.Error("empty chunk must error")
	}
	var bmcErr *bmc.Error
	if !errors.As(err, &bmcErr) || bmcErr.Kind != bmc.KindProtocolError {
		t.Errorf("empty chunk err = %v, want BMC_PROTOCOL_ERROR", err)
	}
}

func TestFRUAreaExtent(t *testing.T) {
	area := r710FRU()
	if e := fruAreaExtent(area[:8], chunkReader(area)); e != len(area) {
		t.Errorf("extent = %d, want full area %d", e, len(area))
	}
	// Unusable header: extent 0 so the caller falls back to the declared size.
	headless := make([]byte, 8)
	headless[0] = 0x01 // board/product pointers zero
	if e := fruAreaExtent(headless, chunkReader(headless)); e != 8 {
		t.Errorf("pointerless header extent = %d, want header size 8", e)
	}
	if e := fruAreaExtent(area[:7], chunkReader(area)); e != 0 {
		t.Errorf("short header extent = %d, want 0", e)
	}
}

func TestFRUIdentity(t *testing.T) {
	model, serial := fruIdentity(r710FRU())
	if model != "PowerEdge R710" {
		t.Errorf("model = %q, want board product", model)
	}
	if serial != "23Q683X" {
		t.Errorf("serial = %q, want product serial over board serial", serial)
	}
	// Board-serial fallback when the product area carries no serial.
	area := buildFRU(
		[]fruField{ascii("DELL"), ascii("R640"), ascii("BOARD01")},
		[]fruField{ascii("DELL"), ascii("host")},
	)
	if _, serial := fruIdentity(area); serial != "BOARD01" {
		t.Errorf("fallback serial = %q, want board serial", serial)
	}
	// Unparseable area: empty identity, no panic.
	if m, s := fruIdentity([]byte{0x01}); m != "" || s != "" {
		t.Errorf("garbage identity = %q/%q, want empty", m, s)
	}
}

func TestFRUHardwareView(t *testing.T) {
	hw := fruHardwareView(r710FRU(), nil)
	if hw.SerialNumber != "23Q683X" {
		t.Errorf("SerialNumber = %q", hw.SerialNumber)
	}
	if hw.Coverage != bmc.CoveragePartial {
		t.Errorf("Coverage = %q, want partial", hw.Coverage)
	}
	if len(hw.CoverageNotes) != 1 {
		t.Fatalf("CoverageNotes = %v, want exactly the fru-limited note", hw.CoverageNotes)
	}

	// A failed read stays green but says so on the record.
	hw = fruHardwareView(nil, errors.New("timeout"))
	if hw.SerialNumber != "" {
		t.Errorf("SerialNumber on failed read = %q", hw.SerialNumber)
	}
	if hw.Coverage != bmc.CoveragePartial || len(hw.CoverageNotes) != 2 {
		t.Errorf("failed read view = %q %v, want partial with 2 notes", hw.Coverage, hw.CoverageNotes)
	}
}
