package builder

// cpio "newc" (070701) read/write for the probe's miniproot segment: the
// kernel concatenates initramfs archives in order and later files override
// earlier ones, so a small appended segment can add (or replace) paths in
// the booted root — see miniprootEntries.

import (
	"bytes"
	"fmt"
	"strconv"
	"strings"
)

const cpioTrailer = "TRAILER!!!"

// cpioReadNewc parses a newc-format cpio archive (plain or gzip) into
// entries. Concatenated archives (the initramfs standard) parse as one
// stream — later entries with the same name override earlier ones in the
// caller's eyes, mirroring how the kernel unpacks them. Stops at the
// trailer; directories come back with a trailing slash in Name.
func cpioReadNewc(data []byte) ([]cpioEntry, error) {
	pad4 := func(n int) int { return (n + 3) &^ 3 }
	var out []cpioEntry
	i := 0
	for {
		if i+110 > len(data) {
			return out, fmt.Errorf("cpio: truncated header at offset %d", i)
		}
		magic := string(data[i : i+6])
		if magic != "070701" && magic != "070702" {
			return out, fmt.Errorf("cpio: bad magic %q at offset %d", magic, i)
		}
		hex := func(off int) int64 {
			v, _ := strconv.ParseInt(string(data[i+off:i+off+8]), 16, 64)
			return v
		}
		mode := hex(14)
		filesize := int(hex(54))
		namesize := int(hex(94))
		nameEnd := i + 110 + namesize
		if nameEnd > len(data) {
			return out, fmt.Errorf("cpio: truncated name at offset %d", i)
		}
		name := strings.TrimRight(string(data[i+110:nameEnd]), "\x00")
		dataStart := pad4(nameEnd)
		if dataStart+filesize > len(data) {
			return out, fmt.Errorf("cpio: truncated data for %q at offset %d", name, i)
		}
		if name == cpioTrailer {
			return out, nil
		}
		e := cpioEntry{Name: name, Mode: mode}
		switch {
		case mode&0o170000 == 0o120000:
			e.Link = strings.TrimRight(string(data[dataStart:dataStart+filesize]), "\x00")
		case filesize > 0:
			e.Body = data[dataStart : dataStart+filesize]
		}
		out = append(out, e)
		i = pad4(dataStart + filesize)
	}
}

// cpioWriteNewc packs entries into a newc archive. Directories keep their
// trailing slash (cpio convention), symlinks carry Link as the body, regular
// files carry Body.
func cpioWriteNewc(entries []cpioEntry) []byte {
	var buf bytes.Buffer
	ino := 0
	hdr := func(name string, mode int64, body []byte) {
		ino++
		var h strings.Builder
		h.WriteString("070701")
		fmt.Fprintf(&h, "%08X", ino)
		fmt.Fprintf(&h, "%08X", mode)
		for _, v := range []int64{0, 0, 1, 0, int64(len(body)), 0, 0, 0, 0, int64(len(name) + 1), 0} {
			fmt.Fprintf(&h, "%08X", v)
		}
		buf.WriteString(h.String())
		buf.WriteString(name)
		buf.WriteByte(0)
		// header+name and body both pad the STREAM to 4-byte boundaries —
		// the loop condition must read the current length: a constant
		// expression never changes as bytes are written, which loops
		// forever (OOM-killing the process — first-draft bug, kept here as
		// a comment because it cost three test runs).
		for buf.Len()%4 != 0 {
			buf.WriteByte(0)
		}
		buf.Write(body)
		for buf.Len()%4 != 0 {
			buf.WriteByte(0)
		}
	}
	for _, e := range entries {
		switch {
		case strings.HasSuffix(e.Name, "/"):
			hdr(e.Name, 0o040755, nil)
		case e.Link != "":
			hdr(e.Name, 0o120777, []byte(e.Link))
		default:
			hdr(e.Name, e.Mode, e.Body)
		}
	}
	hdr(cpioTrailer, 0, nil)
	return buf.Bytes()
}
