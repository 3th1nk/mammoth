package builder

import (
	"bytes"
	"compress/gzip"
	"io"
	"os"
	"strings"
	"testing"
)

// readCpioArchive is a minimal newc reader used to verify the writer's
// output field-by-field (interop with the kernel's initramfs loader —
// concatenated cpio members — is validated by the qemu harness; this pins
// the format mechanics).
func readCpioArchive(t *testing.T, b []byte) map[string]cpioEntry {
	t.Helper()
	out := map[string]cpioEntry{}
	pos := 0
	read := func(n int64) []byte {
		if pos+int(n) > len(b) {
			t.Fatalf("archive truncated at %d (want %d more bytes)", pos, n)
		}
		p := b[pos : pos+int(n)]
		pos += int(n)
		return p
	}
	hexField := func() int64 {
		var v int64
		for _, c := range read(8) {
			v <<= 4
			switch {
			case c >= '0' && c <= '9':
				v |= int64(c - '0')
			case c >= 'a' && c <= 'f':
				v |= int64(c-'a') + 10
			default:
				t.Fatalf("bad hex digit %q in header", c)
			}
		}
		return v
	}
	align := func() { pos = (pos + 3) &^ 3 }
	for {
		if pos+6 > len(b) {
			t.Fatal("missing magic")
		}
		if magic := string(b[pos : pos+6]); magic != cpioMagic {
			t.Fatalf("magic %q", magic)
		}
		pos += 6
		_ = hexField() // ino
		mode := hexField()
		_ = hexField() // uid
		_ = hexField() // gid
		_ = hexField() // nlink
		_ = hexField() // mtime
		size := hexField()
		_ = hexField() // devmajor
		_ = hexField() // devminor
		_ = hexField() // rdevmajor
		_ = hexField() // rdevminor
		namesize := hexField()
		_ = hexField() // check
		name := string(read(namesize - 1))
		read(1) // NUL
		align()
		data := read(size)
		align()
		if name == "TRAILER!!!" {
			return out
		}
		out[name] = cpioEntry{Name: name, Mode: mode, Body: data}
	}
}

func TestAppendCpioArchive(t *testing.T) {
	// A pre-existing initramfs prefix must survive untouched — the appended
	// segment is additive, which is the whole contract with the kernel.
	prefix := []byte{0x1f, 0x8b, 0x08, 0x00, 'p', 'r', 'e', 'f', 'i', 'x'}
	path := t.TempDir() + "/initramfs-lts"
	if err := os.WriteFile(path, prefix, 0o644); err != nil {
		t.Fatal(err)
	}
	entries := []cpioEntry{
		{Name: "etc/.default_boot_services", Mode: 0o100644, Body: []byte{}},
		{Name: "etc/local.d/", Mode: 0o040755},
		{Name: "etc/local.d/mammoth-probe.start", Mode: 0o100755, Body: []byte("#!/bin/sh\necho hi\n")},
		{Name: "etc/runlevels/default/local", Mode: 0o120777, Link: "/etc/init.d/local"},
	}
	if err := appendCpioArchive(path, entries); err != nil {
		t.Fatalf("append: %v", err)
	}

	full, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(full, prefix) {
		t.Fatal("appending must not touch the base initramfs bytes")
	}

	// The appended member is one standalone gzip stream; the kernel
	// processes concatenated members, so the segment must decode alone.
	zr, err := gzip.NewReader(bytes.NewReader(full[len(prefix):]))
	if err != nil {
		t.Fatalf("appended segment is not gzip: %v", err)
	}
	zr.Multistream(false) // exactly one member
	raw, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("decompress: %v", err)
	}
	decoded := readCpioArchive(t, raw)
	for _, want := range []string{
		"etc/.default_boot_services", "etc/local.d/",
		"etc/local.d/mammoth-probe.start", "etc/runlevels/default/local",
	} {
		if _, ok := decoded[want]; !ok {
			t.Fatalf("entry %q missing: %v", want, decoded)
		}
	}
	if got := string(decoded["etc/local.d/mammoth-probe.start"].Body); got != "#!/bin/sh\necho hi\n" {
		t.Fatalf("script content: %q", got)
	}
	// Symlink: the data is the target, the mode carries the type bits.
	link := decoded["etc/runlevels/default/local"]
	if string(link.Body) != "/etc/init.d/local" || link.Mode&0o170000 != 0o120000 {
		t.Fatalf("symlink entry wrong: mode=%o body=%q", link.Mode, link.Body)
	}
	// The trailer terminates the archive — nothing may follow it.
	if !strings.Contains(string(raw), "TRAILER!!!") {
		t.Fatal("no trailer")
	}
}
