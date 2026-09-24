// ISO 9660 content sniffing for the artifact library (docs/04-install-spec.md
// §4.1): after the digest-gated download lands, the fetch worker identifies
// the image's distro/version so operators don't have to register them by
// hand. Pure stdlib — the API image is distroless, so no xorriso/isoinfo.
//
// The reader walks the fixed structure: the Primary Volume Descriptor at
// sector 16 (volume id + root directory record), then directory extents
// (byte-packed records that never span a 2048-byte sector), then small
// marker files (.treeinfo, .disk/info, …). Family fingerprints:
//
//	.treeinfo            → anaconda family (Rocky/CentOS/Kylin/UOS/euler…)
//	.discinfo            → anaconda fallback (name + version lines)
//	.disk/info           → debian / ubuntu (casper/ marks ubuntu live)
//	sources/install.wim  → windows
//	apks/ + .alpine-release → alpine
package images

import (
	"encoding/binary"
	"os"
	"regexp"
	"strings"
)

const sectorSize = 2048

// DetectISO inspects an opened ISO image and returns the distro token and
// version it could identify. ok is false when nothing conclusive was found —
// absence is never a verdict; the registrant's own values always win.
func DetectISO(f *os.File) (distro, version string, ok bool) {
	read := func(offset, length int64) ([]byte, error) {
		buf := make([]byte, length)
		if _, err := f.ReadAt(buf, offset); err != nil {
			return nil, err
		}
		return buf, nil
	}

	pvd, err := read(int64(sectorSize*16), sectorSize)
	if err != nil || len(pvd) < sectorSize || pvd[0] != 1 || string(pvd[1:6]) != "CD001" {
		return "", "", false
	}
	volumeID := pvdStr(pvd[40:72])
	volLower := strings.ToLower(volumeID)

	root := parseDirRec(pvd[156:190])
	if root.length == 0 {
		return "", "", false
	}
	entries, err := dirEntries(read, isoEntry{lba: root.lba, size: root.size, isDir: true})
	if err != nil {
		return "", "", false
	}

	// resolve reads a marker file by path ("info" under ".disk" → ".disk/info")
	readFile := func(path string) (string, bool) {
		segs := strings.Split(path, "/")
		cur := entries
		var rec isoEntry
		for i, seg := range segs {
			e, ok := cur[seg]
			if !ok {
				return "", false
			}
			rec = e
			if i < len(segs)-1 {
				if !rec.isDir {
					return "", false
				}
				if cur, err = dirEntries(read, rec); err != nil {
					return "", false
				}
			}
		}
		if rec.isDir {
			return "", false
		}
		data, err := read(int64(rec.lba)*sectorSize, roundSector(rec.size))
		if err != nil {
			return "", false
		}
		n := rec.size
		if int64(len(data)) < n {
			n = int64(len(data))
		}
		return strings.TrimSpace(string(data[:n])), true
	}
	hasDir := func(name string) bool {
		rec, ok := entries[name]
		return ok && rec.isDir
	}

	// 1) .treeinfo — anaconda family, the richest source.
	if body, ok := readFile(".treeinfo"); ok {
		family, ver := treeinfoFields(body)
		if d, ok := familyToDistro(family); ok {
			return d, ver, true
		}
	}
	// 2) .discinfo — anaconda fallback: line1 timestamp, line2 name, line3 version.
	if body, ok := readFile(".discinfo"); ok {
		lines := strings.SplitN(body, "\n", 3)
		if len(lines) >= 3 {
			if d, ok := familyToDistro(lines[1]); ok {
				return d, strings.TrimSpace(lines[2]), true
			}
		}
	}
	// 3) .disk/info — debian and ubuntu media.
	if body, ok := readFile(".disk/info"); ok {
		lower := strings.ToLower(body)
		ver := versionIn(body)
		switch {
		case strings.Contains(lower, "ubuntu"):
			return "ubuntu", ver, true
		case strings.Contains(lower, "debian"):
			return "debian", ver, true
		}
	}
	// 4) sources/ on a non-anaconda ISO — the windows marker.
	if hasDir("sources") {
		return "windows", versionIn(volLower), true
	}
	// 5) alpine.
	if body, ok := readFile(".alpine-release"); ok {
		return "alpine", versionIn(body), true
	}
	// 6) volume-id token fallback (custom-built ISOs often name themselves).
	for _, token := range []string{"rocky", "centos", "kylin", "uniontech", "uos", "ubuntu", "debian", "alpine", "windows"} {
		if strings.Contains(volLower, token) {
			return token, versionIn(volLower), true
		}
	}
	return "", "", false
}

// isoEntry is one resolved directory record.
type isoEntry struct {
	lba   int64
	size  int64
	isDir bool
}

type dirRec struct {
	lba    int64
	size   int64
	length byte
	isDir  bool
	name   string
}

func parseDirRec(b []byte) dirRec {
	if len(b) < 34 {
		return dirRec{}
	}
	nameLen := int(b[32])
	name := ""
	if 33+nameLen <= len(b) {
		name = pvdStr(b[33 : 33+nameLen])
	}
	return dirRec{
		lba:    int64(binary.LittleEndian.Uint32(b[2:6])),
		size:   int64(binary.LittleEndian.Uint32(b[10:14])),
		length: b[0],
		isDir:  b[25]&2 != 0,
		name:   name,
	}
}

// dirEntries walks a directory extent into "name → entry". Names are
// lower-cased with the ";1" ISO version suffix and trailing dots stripped;
// the "." and ".." records are skipped. Records never span sectors — sector
// padding is skipped wholesale.
func dirEntries(read func(offset, length int64) ([]byte, error), rec isoEntry) (map[string]isoEntry, error) {
	data, err := read(int64(rec.lba)*sectorSize, roundSector(rec.size))
	if err != nil {
		return nil, err
	}
	out := map[string]isoEntry{}
	off := 0
	for off < len(data) {
		if data[off] == 0 {
			off = (off/sectorSize + 1) * sectorSize
			continue
		}
		length := int(data[off])
		if length == 0 || off+length > len(data) {
			break
		}
		rec := parseDirRec(data[off : off+length])
		if debugDir {
			off2 := off
			t := "F"
			if rec.isDir {
				t = "D"
			}
			_ = off2
			println("DIRREC off=", off2, " len=", length, " type=", t, " nameLen=", rec.name, " name=", rec.name)
		}
		off += length
		if rec.name == "" || rec.name == "." || rec.name == ".." {
			continue
		}
		out[strings.ToLower(rec.name)] = isoEntry{lba: rec.lba, size: rec.size, isDir: rec.isDir}
	}
	return out, nil
}

// pvdStr strips NUL padding, the ";1" ISO version suffix, and trailing dots.
func pvdStr(b []byte) string {
	if n := strings.IndexByte(string(b), 0); n >= 0 {
		b = b[:n]
	}
	clean := strings.TrimSuffix(strings.TrimSpace(string(b)), ";1")
	return strings.TrimRight(clean, ".")
}

var debugDir = os.Getenv("MAMMOTH_DEBUG_DIR") != ""

var versionRe = regexp.MustCompile(`\d+(\.\d+)*`)

func versionIn(s string) string {
	return versionRe.FindString(s)
}

// treeinfoFields picks family/version out of the [general] section.
func treeinfoFields(body string) (family, version string) {
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if i := strings.Index(line, "="); i > 0 {
			key := strings.TrimSpace(line[:i])
			val := strings.Trim(strings.TrimSpace(line[i+1:]), `"'`)
			switch key {
			case "family":
				family = val
			case "version":
				version = val
			}
		}
	}
	return family, version
}

// familyToDistro maps an anaconda family string to the distro token the
// library stores. Unknown families are rejected (absence is not a verdict).
func familyToDistro(family string) (string, bool) {
	if family == "" {
		return "", false
	}
	f := strings.ToLower(family)
	switch {
	case strings.Contains(f, "rocky"):
		return "rocky", true
	case strings.Contains(f, "centos"):
		return "centos", true
	case strings.Contains(f, "kylin"):
		return "kylin", true
	case strings.Contains(f, "uniontech"), strings.Contains(f, "uos"):
		return "uos", true
	case strings.Contains(f, "euler"):
		return "openeuler", true
	case strings.Contains(f, "fedora"):
		return "fedora", true
	}
	return "", false
}

func roundSector(n int64) int64 {
	if n <= 0 {
		return sectorSize
	}
	return ((n + sectorSize - 1) / sectorSize) * sectorSize
}
