package images

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// buildTestISO synthesizes a minimal-but-spec-shaped ISO 9660 image: PVD at
// sector 16, root directory extent at sector 20, entries laid out in
// deterministic two-phase allocation (LBA first, extents rendered after).
type isoFile struct {
	path    string // "treeinfo", ".disk/info", "sources" — segments, no ";1"
	content string
	isDir   bool
}

func buildTestISO(t *testing.T, volumeID string, entries []isoFile) []byte {
	t.Helper()
	const (
		pvdSector  = 16
		rootSector = 20
	)
	sectors := map[int][]byte{}
	next := rootSector + 1

	// phase 1: allocate an LBA per entry; file sizes are static and known
	// upfront (dir extents only reference the LBA, so this order is safe)
	alloc := map[string][2]int64{}
	for _, e := range entries {
		size := int64(0)
		if !e.isDir {
			size = int64(len(e.content))
		}
		alloc[e.path] = [2]int64{int64(next), size}
		next++
	}
	sizeOf := func(path string, data []byte) [2]int64 {
		a := alloc[path]
		a[1] = int64(len(data))
		alloc[path] = a
		return a
	}

	// phase 2: render every extent at its pre-allocated LBA
	for _, e := range entries {
		var data []byte
		if e.isDir {
			data = dirDataFor(entries, alloc, e.path)
		} else {
			data = []byte(e.content)
		}
		a := sizeOf(e.path, data)
		sectors[int(a[0])] = data
	}

	// root extent last: every child record reads alloc
	rootData := dirDataFor(entries, alloc, "")
	sectors[rootSector] = rootData

	// PVD with the root record
	pvd := make([]byte, sectorSize)
	pvd[0] = 1
	copy(pvd[1:6], "CD001")
	pvd[6] = 1
	copy(pvd[40:72], volumeID)
	rec := make([]byte, 34)
	rec[0] = 34
	binary.LittleEndian.PutUint32(rec[2:6], uint32(rootSector))
	binary.LittleEndian.PutUint32(rec[10:14], uint32(len(rootData)))
	rec[25] = 2
	rec[32] = 1
	copy(pvd[156:190], rec)
	sectors[pvdSector] = pvd

	maxS := 0
	for s := range sectors {
		if s > maxS {
			maxS = s
		}
	}
	out := make([]byte, (maxS+1)*sectorSize)
	for s, b := range sectors {
		copy(out[s*sectorSize:], b)
	}
	return out
}

// dirDataFor renders the directory extent listing entries directly under
// path ("" = root), including the . and .. records.
func dirDataFor(entries []isoFile, alloc map[string][2]int64, path string) []byte {
	var rows [][]byte
	dot := make([]byte, 34)
	dot[0] = 34
	dot[25] = 2
	dot[32] = 1
	rows = append(rows, dot, append([]byte{}, dot...))

	seen := map[string]bool{}
	for _, e := range entries {
		name := e.path
		if path != "" {
			if !strings.HasPrefix(name, path+"/") {
				continue
			}
			name = strings.TrimPrefix(name, path+"/")
		}
		if strings.Contains(name, "/") {
			continue // deeper level: belongs to a child extent
		}
		if seen[name] {
			continue
		}
		seen[name] = true
		a, ok := alloc[e.path]
		if !ok {
			continue
		}
		rec := make([]byte, 34)
		rec[0] = 34
		binary.LittleEndian.PutUint32(rec[2:6], uint32(a[0]))
		binary.LittleEndian.PutUint32(rec[10:14], uint32(a[1]))
		if e.isDir {
			rec[25] = 2
		}
		nm := name
		if !e.isDir && !strings.Contains(nm, ".") {
			nm += ";1" // ISO level-1 file version suffix
		}
		rec[32] = byte(len(nm))
		rec = rec[:33] // name 紧跟 nameLen（append 会在 34 位留一个零字节间隙）
		rec = append(rec, nm...)
		rec[0] = byte(len(rec))
		rows = append(rows, rec)
	}

	var out []byte
	for _, r := range rows {
		out = append(out, r...)
	}
	for len(out)%sectorSize != 0 {
		out = append(out, 0)
	}
	return out
}

func writeISO(t *testing.T, img []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "test.iso")
	if err := os.WriteFile(p, img, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestDetectISOFamilies(t *testing.T) {
	cases := []struct {
		name       string
		volumeID   string
		entries    []isoFile
		wantDistro string
		wantVer    string
	}{
		{
			name:     "rocky treeinfo",
			volumeID: "Rocky-9-4-x86_64-dvd",
			entries: []isoFile{
				{path: ".treeinfo", content: "[general]\nfamily = Rocky Linux\nversion = 9.4\narch = x86_64\n"},
			},
			wantDistro: "rocky", wantVer: "9.4",
		},
		{
			name:     "kylin discinfo",
			volumeID: "Kylin-Server-V10",
			entries: []isoFile{
				{path: ".discinfo", content: "1234567890.000000\nKylin Server V10 SP3\nV10\n"},
			},
			wantDistro: "kylin", wantVer: "V10",
		},
		{
			name:     "ubuntu diskinfo with casper",
			volumeID: "Ubuntu-Server 24.04",
			entries: []isoFile{
				{path: "casper", isDir: true},
				{path: ".disk", isDir: true},
				{path: ".disk/info", content: "Ubuntu-Server 24.04.1 LTS \"Noble Numbat\" - Release amd64\n"},
			},
			wantDistro: "ubuntu", wantVer: "24.04.1",
		},
		{
			name:     "debian diskinfo",
			volumeID: "Debian 12.5.0 amd64",
			entries: []isoFile{
				{path: ".disk/info", content: "Debian GNU/Linux 12.5.0 \"Bookworm\" - Official amd64 NETINST\n"},
			},
			wantDistro: "debian", wantVer: "12.5.0",
		},
		{
			name:     "windows sources dir",
			volumeID: "SERVER2019",
			entries: []isoFile{
				{path: "sources", isDir: true},
			},
			wantDistro: "windows", wantVer: "2019",
		},
		{
			name:     "alpine release",
			volumeID: "alpine-virt 3.20",
			entries: []isoFile{
				{path: "apks", isDir: true},
				{path: ".alpine-release", content: "3.20.3\n"},
			},
			wantDistro: "alpine", wantVer: "3.20.3",
		},
		{
			name:       "unknown iso stays unknown",
			volumeID:   "RANDOM_DATA_DISC",
			entries:    []isoFile{{path: "data", isDir: true}},
			wantDistro: "", wantVer: "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := writeISO(t, buildTestISO(t, tc.volumeID, tc.entries))
			f, err := os.Open(p)
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			distro, version, ok := DetectISO(f)
			if tc.wantDistro == "" {
				if ok {
					t.Fatalf("detected %q/%q, want unknown", distro, version)
				}
				return
			}
			if !ok {
				t.Fatalf("not detected, want %q/%q", tc.wantDistro, tc.wantVer)
			}
			if distro != tc.wantDistro || version != tc.wantVer {
				t.Errorf("detected %q/%q, want %q/%q", distro, version, tc.wantDistro, tc.wantVer)
			}
		})
	}
}
