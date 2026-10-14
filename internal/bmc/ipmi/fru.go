// FRU reads over the raw Storage commands (IPMI 2.0 §34) plus the standard
// FRU layout parse. goipmi ships neither the constants nor the types, so
// they are defined here — the same raw-Send pattern powerState and
// SetBootDevice already use. This is the FRU-limited inventory slice
// docs/07-bmc.md §2 promises for IPMI machines (Redfish-less controllers
// like the iDRAC6 included): chassis identity, not component depth.
package ipmi

import (
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	goipmi "github.com/vmware/goipmi"

	"github.com/3th1nk/mammoth/internal/bmc"
)

const (
	// netFnStorage is the IPMI Storage network function (IPMI 2.0 §5.1);
	// goipmi only predefines Chassis and App.
	netFnStorage = goipmi.NetworkFunction(0x0a)

	commandGetFRUInventoryArea = goipmi.Command(0x10)
	commandReadFRUData         = goipmi.Command(0x11)

	// fruDeviceID 0 is the board/product FRU every spec-compliant BMC
	// implements; PSU and chassis FRUs live on higher IDs and have no slot
	// in the inventory model.
	fruDeviceID = 0

	// maxFRUChunk bounds one Read FRU Data exchange. Two real-world ceilings
	// stack up: iDRAC6 answers 0xCA above 16 bytes, and the lanplus
	// transport's ipmitool output wraps every 16 bytes while goipmi's raw
	// decoder panics on the embedded newline — so a response (count byte +
	// payload) must stay within one 16-column line: 1 + 15. The read loop
	// additionally halves the chunk on completion-code rejections down to
	// minFRUChunk.
	maxFRUChunk = 15
	minFRUChunk = 4

	// maxFRUBytes is a sanity bound on the area length; real BMCs ship
	// kilobytes and a wedged firmware must not allocate megabytes.
	maxFRUBytes = 64 * 1024
)

type fruAreaInfoRequest struct{ DeviceID uint8 }

// fruAreaInfoResponse per IPMI 2.0 §34.2: area size and addressing mode.
// Hand-unmarshaled because real firmware trims the response: iDRAC6 ships
// three bytes (no access byte) — access defaults to byte mode there.
type fruAreaInfoResponse struct {
	goipmi.CompletionCode
	SizeLSB uint8
	SizeMSB uint8
	Access  uint8 // 0 = byte-addressed, 1 = word-addressed
}

func (r *fruAreaInfoResponse) UnmarshalBinary(buf []byte) error {
	if len(buf) < 3 {
		return goipmi.ErrShortPacket
	}
	r.CompletionCode = goipmi.CompletionCode(buf[0])
	r.SizeLSB = buf[1]
	r.SizeMSB = buf[2]
	if len(buf) >= 4 {
		r.Access = buf[3]
	}
	return nil
}

type fruReadRequest struct {
	DeviceID uint8
	Offset   uint16 // in the BMC's addressing unit (byte or word)
	Count    uint8  // in the same unit
}

// fruReadResponse per IPMI 2.0 §34.3: variable length, so both directions
// are hand-marshaled (binary.Write cannot encode the Data slice).
type fruReadResponse struct {
	goipmi.CompletionCode
	Count uint8
	Data  []byte
}

func (r *fruReadResponse) MarshalBinary() ([]byte, error) {
	buf := make([]byte, 2+len(r.Data))
	buf[0] = byte(r.CompletionCode.Code())
	buf[1] = byte(len(r.Data))
	copy(buf[2:], r.Data)
	return buf, nil
}

func (r *fruReadResponse) UnmarshalBinary(buf []byte) error {
	if len(buf) < 2 {
		return goipmi.ErrShortPacket
	}
	r.CompletionCode = goipmi.CompletionCode(buf[0])
	r.Count = buf[1]
	if n := len(buf) - 2; int(r.Count) > n {
		r.Count = uint8(n) // tolerate firmware over-reporting the count
	}
	r.Data = append(r.Data[:0], buf[2:2+int(r.Count)]...)
	return nil
}

// readFRUArea fetches the effective FRU inventory area of a device. All
// addressing below is bytes; the driver-side closure hides whether the BMC
// speaks byte or word units (access bit).
//
// The effective length comes from the common header's own area pointers,
// not from the controller-declared size: real firmware lies about the size
// (measured iDRAC6 reports 16 while the areas reach past byte 359), and a
// wrong-but-plausible declared size would either truncate the read or pad
// it with hundreds of wasted round trips.
func readFRUArea(client *goipmi.Client, deviceID uint8) ([]byte, error) {
	info := &fruAreaInfoResponse{}
	req := &goipmi.Request{
		NetworkFunction: netFnStorage,
		Command:         commandGetFRUInventoryArea,
		Data:            &fruAreaInfoRequest{DeviceID: deviceID},
	}
	if err := client.Send(req, info); err != nil {
		return nil, bmc.Classify("fru_area_info", err)
	}
	unit := 1
	if info.Access&1 == 1 {
		unit = 2
	}
	raw := func(offset, count uint16) ([]byte, error) {
		res := &fruReadResponse{}
		req := &goipmi.Request{
			NetworkFunction: netFnStorage,
			Command:         commandReadFRUData,
			Data: &fruReadRequest{
				DeviceID: deviceID,
				Offset:   offset,
				Count:    uint8(count),
			},
		}
		if err := client.Send(req, res); err != nil {
			return nil, bmc.Classify("fru_read", err)
		}
		return res.Data, nil
	}
	read := func(byteOffset, byteCount uint16) ([]byte, error) {
		return raw(byteOffset/uint16(unit), byteCount/uint16(unit))
	}

	head, err := read(0, 8)
	if err != nil {
		return nil, err
	}
	total := (int(info.SizeMSB)<<8 | int(info.SizeLSB)) * unit
	if e := fruAreaExtent(head, read); e > 0 {
		total = e
	}
	return readFRUChunks(read, total)
}

// fruAreaExtent derives the effective FRU length from the common header:
// follow the board/product pointers, peek each area's own length byte, and
// return the furthest end. Zero when the header is unusable — the caller
// then falls back to the controller-declared size.
func fruAreaExtent(hdr []byte, peek func(byteOffset, byteCount uint16) ([]byte, error)) int {
	if len(hdr) < 8 || hdr[0] != 0x01 || hdr[7] != 0x00 {
		return 0
	}
	end := 8
	for _, idx := range []int{3, 4} { // board, product pointers
		start := int(hdr[idx]) * 8
		if start == 0 || start+2 > maxFRUBytes {
			continue
		}
		h, err := peek(uint16(start), 2)
		if err != nil || len(h) < 2 || h[0] != 0x01 {
			continue
		}
		if e := start + int(h[1])*8; e > end {
			end = e
		}
	}
	return end
}

// rspCCRe extracts the completion code from a lanplus-transport failure:
// goipmi's lanplus transport shells out to ipmitool (decision D5's
// no-shell-out intent is defeated upstream) and only leaves the code as
// "rsp=0x.." text inside the child's stderr.
var rspCCRe = regexp.MustCompile(`rsp=0x([0-9a-fA-F]{2})`)

// completionCodeOf reports the IPMI completion code carried by an exchange
// failure, whether it surfaces typed (pure-Go lan transport) or as ipmitool
// stderr text (lanplus transport).
func completionCodeOf(err error) (goipmi.CompletionCode, bool) {
	var cc goipmi.CompletionCode
	if errors.As(err, &cc) {
		return cc, true
	}
	if m := rspCCRe.FindStringSubmatch(err.Error()); m != nil {
		if v, perr := strconv.ParseUint(m[1], 16, 8); perr == nil {
			return goipmi.CompletionCode(v), true
		}
	}
	return 0, false
}

// readFRUChunks drives the chunked Read FRU Data loop over byte-addressed
// reads. A completion-code rejection (firmware unwilling to return the
// requested length) halves the chunk and retries the same offset down to
// minFRUChunk; anything else — and anything still failing at the floor —
// is fatal.
func readFRUChunks(read func(offset, count uint16) ([]byte, error), total int) ([]byte, error) {
	if total <= 0 || total > maxFRUBytes {
		return nil, &bmc.Error{Kind: bmc.KindProtocolError, Op: "fru_area_info",
			Detail: fmt.Sprintf("implausible FRU area size %d bytes", total)}
	}
	chunk := uint16(maxFRUChunk)
	area := make([]byte, 0, total)
	for len(area) < total {
		n := chunk
		if remain := uint16(total - len(area)); n > remain {
			n = remain
		}
		c, err := read(uint16(len(area)), n)
		if err != nil {
			if _, isCC := completionCodeOf(err); isCC && chunk > minFRUChunk {
				chunk /= 2
				if chunk < minFRUChunk {
					chunk = minFRUChunk
				}
				continue
			}
			return nil, err
		}
		if len(c) == 0 {
			return nil, &bmc.Error{Kind: bmc.KindProtocolError, Op: "fru_read",
				Detail: fmt.Sprintf("empty FRU read at byte offset %d", len(area))}
		}
		area = append(area, c...)
	}
	return area, nil
}

// fruIdentity extracts what Probe backfills into BMCInfo: the board area's
// product name as model, the product area's serial (the service tag on
// Dell) with the board serial as fallback.
func fruIdentity(area []byte) (model, serial string) {
	fru, err := parseFRU(area)
	if err != nil {
		return "", ""
	}
	return fru.BoardProduct, firstNonEmpty(fru.ProductSerial, fru.BoardSerial)
}

// fruHardwareView renders the FRU-limited inventory view. It never fails the
// discovery over a missing slice — a failed or unparseable read becomes a
// coverage note, and the standard-IPMI blind spots are always on record.
func fruHardwareView(area []byte, readErr error) bmc.HardwareView {
	hw := bmc.HardwareView{}
	switch {
	case readErr != nil:
		hw.Note("fru read failed: " + readErr.Error())
	default:
		if fru, perr := parseFRU(area); perr != nil {
			hw.Note("fru parse failed: " + perr.Error())
		} else {
			hw.SerialNumber = firstNonEmpty(fru.ProductSerial, fru.BoardSerial)
		}
	}
	hw.Note("ipmi: fru-limited inventory — cpu/memory/disks/nics unavailable over standard IPMI")
	return hw
}

// fruInfo is the identity subset mammoth reads out of the FRU board and
// product areas.
type fruInfo struct {
	BoardProduct  string // board area, product name (e.g. "PowerEdge R710")
	BoardSerial   string // board area, serial number
	ProductName   string // product area, product name (often the host label)
	ProductSerial string // product area, serial number (the service tag on Dell)
}

// parseFRU walks the standard FRU information layout (FRU Information
// Storage Definition v1.3): a common header pointing at the board/product
// areas, each a run of type-length fields. Deliberately lenient — area
// checksums are not enforced and a truncated field ends the walk, because
// partial identity beats no identity on misbehaving firmware.
func parseFRU(area []byte) (fruInfo, error) {
	if len(area) < 8 {
		return fruInfo{}, fmt.Errorf("fru area too short (%d bytes)", len(area))
	}
	if area[0] != 0x01 {
		return fruInfo{}, fmt.Errorf("unsupported fru format version %#x", area[0])
	}
	var info fruInfo
	// Board area: version, length, 3-byte mfg datetime, then fields in the
	// order mfg, product name, serial, part number, ... Product area:
	// version, length, then mfg, name, part, version, serial, ...
	if off := int(area[3]) * 8; off > 0 && off+2 <= len(area) {
		if fields := fruFields(area, off, 5); len(fields) > 2 {
			info.BoardProduct = fields[1]
			info.BoardSerial = fields[2]
		}
	}
	if off := int(area[4]) * 8; off > 0 && off+2 <= len(area) {
		if fields := fruFields(area, off, 2); len(fields) > 4 {
			info.ProductName = fields[1]
			info.ProductSerial = fields[4]
		}
	}
	return info, nil
}

// fruFields decodes one area's type-length run; start skips the area header
// (version+length, plus the mfg datetime on board areas). Positional order
// is what the spec fixes, so empty fields still occupy their slot.
func fruFields(area []byte, off, start int) []string {
	end := off + int(area[off+1])*8
	if end > len(area) {
		end = len(area)
	}
	p := off + start
	var fields []string
	for p < end {
		tl := area[p]
		if tl == 0xC1 { // end of fields marker
			break
		}
		n := int(tl & 0x3f)
		p++
		if p+n > end {
			break
		}
		fields = append(fields, fruDecode(area[p:p+n], int(tl>>6)))
		p += n
	}
	return fields
}

// fruDecode renders one field per its type code: 0 binary → hex, 1 BCD plus
// → decimal digits only (punctuation dropped — cosmetic for identity use),
// 2 six-bit packed ASCII, 3 eight-bit ASCII. Fixed-width firmware fields
// pad with trailing blanks (the real R710 carries "bluefish" as 31 bytes),
// so ASCII fields are right-trimmed.
func fruDecode(b []byte, typ int) string {
	switch typ {
	case 0:
		return hex.EncodeToString(b)
	case 2:
		return decode6bitASCII(b)
	case 3:
		return strings.TrimRight(string(b), " \x00")
	default: // BCD plus
		out := make([]byte, 0, len(b)*2)
		for _, c := range b {
			if hi := c >> 4; hi <= 9 {
				out = append(out, '0'+hi)
			}
			if lo := c & 0x0f; lo <= 9 {
				out = append(out, '0'+lo)
			}
		}
		return string(out)
	}
}

// decode6bitASCII unpacks a FRU type-2 field: characters (biased by 0x20)
// pack six bits at a time, three bytes carrying four characters.
func decode6bitASCII(b []byte) string {
	out := make([]byte, 0, len(b))
	for i := 0; i+3 <= len(b); i += 3 {
		c0 := b[i] & 0x3f
		c1 := ((b[i] & 0xc0) >> 6) | ((b[i+1] & 0x0f) << 2)
		c2 := ((b[i+1] & 0xf0) >> 4) | ((b[i+2] & 0x03) << 4)
		c3 := (b[i+2] & 0xfc) >> 2
		out = append(out, c0+0x20, c1+0x20, c2+0x20, c3+0x20)
	}
	return string(out)
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
