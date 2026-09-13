package builder

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"os"
)

// cpioEntry is one file in a "newc"-format cpio archive — the initramfs
// format. Directories carry a trailing slash in Name and no Body; symlinks
// carry Link and no Body.
type cpioEntry struct {
	Name string
	Mode int64
	Link string
	Body []byte
}

// cpioHeader is the fixed magic of the newc ("cpio -H newc") format.
const cpioMagic = "070701"

// appendCpioArchive builds a newc archive of entries, gzip-compresses it,
// and appends it to initrd (a gzip'd cpio). The kernel's initramfs loader
// processes concatenated cpio segments — each independently compressed —
// which is the same mechanism early-microcode images use, so the appended
// overlay lands on top of the base tree without repacking it.
func appendCpioArchive(initrd string, entries []cpioEntry) error {
	var cpio bytes.Buffer
	for _, e := range entries {
		// writeCpioHeader emits header + name (+ pad); only the data part
		// follows here. Symlinks carry the target as their data; directories
		// carry nothing.
		if err := writeCpioHeader(&cpio, e); err != nil {
			return err
		}
		if e.Link != "" {
			cpio.WriteString(e.Link)
			padAlign(&cpio, 4)
		} else if !isDir(e) {
			cpio.Write(e.Body)
			padAlign(&cpio, 4)
		}
	}
	// Trailer entry terminates the archive.
	if err := writeCpioHeader(&cpio, cpioEntry{Name: "TRAILER!!!"}); err != nil {
		return err
	}
	padAlign(&cpio, 4)

	var gz bytes.Buffer
	w := gzip.NewWriter(&gz)
	if _, err := w.Write(cpio.Bytes()); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	f, err := os.OpenFile(initrd, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(gz.Bytes())
	return err
}

func isDir(e cpioEntry) bool {
	return e.Mode&0o170000 == 0o040000
}

// writeCpioHeader emits the 110-byte newc header (magic + 13 × 8-hex-digit
// ASCII fields) plus the NUL-terminated name, padded to 4.
func writeCpioHeader(w *bytes.Buffer, e cpioEntry) error {
	size := int64(len(e.Body))
	if e.Link != "" {
		size = int64(len(e.Link))
	}
	if isDir(e) {
		size = 0
	}
	namesize := int64(len(e.Name) + 1)
	var b bytes.Buffer
	put := func(v int64) {
		const hexdigits = "0123456789abcdef"
		var s [8]byte
		vv := uint64(v)
		for i := 7; i >= 0; i-- {
			s[i] = hexdigits[vv&0xf]
			vv >>= 4
		}
		b.Write(s[:])
	}
	put(0)             // ino (untracked — the base archive owns inode space)
	put(int64(e.Mode)) // mode
	put(0)             // uid
	put(0)             // gid
	put(1)             // nlink
	put(0)             // mtime
	put(size)          // filesize
	put(0)             // devmajor
	put(0)             // devminor
	put(0)             // rdevmajor
	put(0)             // rdevminor
	put(namesize)      // namesize (incl. NUL)
	put(0)             // check
	if b.Len() != 104 {
		return fmt.Errorf("builder: cpio header is %d bytes, want 104", b.Len())
	}
	w.WriteString(cpioMagic)
	w.Write(b.Bytes())
	w.WriteString(e.Name)
	w.WriteByte(0)
	padAlign(w, 4)
	return nil
}

// padAlign advances w to the next multiple of n (newc pads header, name and
// data to 4-byte boundaries).
func padAlign(w *bytes.Buffer, n int) {
	for w.Len()%n != 0 {
		w.WriteByte(0)
	}
}
