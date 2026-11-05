package inbandssh

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// fakeRunner serves recorded outputs — the golden fixtures pin the parse
// pipeline against the shape a real machine produces (acceptance: "对一台
// 运行中的机器采集出与人工 lsblk 一致的分区快照").
type fakeRunner struct {
	out []byte
	err error
}

func (f fakeRunner) Run(_ context.Context, _ string, _ Credentials, _ string) ([]byte, error) {
	return f.out, f.err
}

// fixture mirrors a two-disk server: NVMe with GPT (EFI + LVM-ish xfs root),
// SATA HDD with MBR, loop device that must be skipped, two NICs (one up).
const fixture = `{
  "blockdevices": [
    {
      "name": "nvme0n1", "path": "/dev/nvme0n1", "size": 500107862016, "type": "disk",
      "serial": "S6XPN0001", "pttype": "GPT",
      "children": [
        {"name": "nvme0n1p1", "path": "/dev/nvme0n1p1", "size": 536870912, "type": "part",
         "fstype": "vfat", "partlabel": "EFI", "partuuid": "1111-2222", "mountpoint": "/boot/efi"},
        {"name": "nvme0n1p2", "path": "/dev/nvme0n1p2", "size": 499569602560, "type": "part",
         "fstype": "xfs", "mountpoint": "/"}
      ]
    },
    {
      "name": "sda", "path": "/dev/sda", "size": 1000204886016, "type": "disk",
      "serial": "GIM256_2021", "pttype": "dos",
      "children": [
        {"name": "sda1", "path": "/dev/sda1", "size": 1000204879872, "type": "part",
         "fstype": "ext4", "mountpoint": "/data"}
      ]
    },
    {"name": "loop0", "path": "/dev/loop0", "size": 4096, "type": "loop"}
  ]
}
===BLKID===
/dev/nvme0n1p1: UUID="F8A1-9C03" TYPE="vfat" PARTUUID="1111-2222" PARTLABEL="EFI"
/dev/nvme0n1p2: UUID="9f86d081-884c-7d65-9a2f-aaa63c07a73b" TYPE="xfs"
/dev/sda1: UUID="b2a1c3d4-0000-1111-2222-333344445555" TYPE="ext4"
===LINK===
[{"ifindex":1,"ifname":"lo","flags":["LOOPBACK","UP"],"mtu":65536,"operstate":"UNKNOWN","address":"00:00:00:00:00:00"},
 {"ifindex":2,"ifname":"eno1","flags":["BROADCAST","MULTICAST","UP"],"mtu":1500,"operstate":"UP","address":"aa:bb:cc:dd:ee:01"},
 {"ifindex":3,"ifname":"eno2","flags":["BROADCAST","MULTICAST"],"mtu":1500,"operstate":"DOWN","address":"aa:bb:cc:dd:ee:02"}]
===PARTS===
/sys/block/nvme0n1/nvme0n1p1/start 2048
/sys/block/nvme0n1/nvme0n1p2/start 1050624
/sys/block/sda/sda1/start 2048
`

func TestCollectGolden(t *testing.T) {
	c := &Collector{Runner: fakeRunner{out: []byte(fixture)}}
	res, err := c.Collect(context.Background(), "10.0.0.5", Credentials{Username: "root", Password: "x"})
	if err != nil {
		t.Fatalf("collect: %v", err)
	}

	l := res.Layout
	if l.Source != "inband_ssh" {
		t.Fatalf("source = %q", l.Source)
	}
	if l.CapturedAt.IsZero() {
		t.Fatal("captured_at must be set")
	}

	if len(l.Disks) != 2 {
		t.Fatalf("loop device must be skipped; disks = %d", len(l.Disks))
	}

	nvme := l.Disks[0]
	if nvme.Device != "/dev/nvme0n1" || nvme.Table != "gpt" {
		t.Fatalf("nvme disk header wrong: %+v", nvme)
	}
	if nvme.Match["serial"] != "S6XPN0001" {
		t.Fatalf("match.serial missing: %+v", nvme.Match)
	}
	if len(nvme.Partitions) != 2 {
		t.Fatalf("want 2 partitions, got %d", len(nvme.Partitions))
	}
	// The essence of "matches a manual lsblk": offsets and sizes in bytes,
	// partition numbers derived correctly, blkid UUID preferred.
	p1, p2 := nvme.Partitions[0], nvme.Partitions[1]
	if p1.Number != 1 || p1.StartBytes != 2048*512 || p1.EndBytes != 2048*512+536870912-1 {
		t.Fatalf("p1 geometry wrong: %+v", p1)
	}
	if p1.UUID != "F8A1-9C03" || p1.FSType != "vfat" || p1.Label != "EFI" || p1.Mountpoint != "/boot/efi" {
		t.Fatalf("p1 identity wrong: %+v", p1)
	}
	if p2.Number != 2 || p2.StartBytes != 1050624*512 || p2.UUID != "9f86d081-884c-7d65-9a2f-aaa63c07a73b" {
		t.Fatalf("p2 wrong: %+v", p2)
	}

	sda := l.Disks[1]
	if sda.Table != "mbr" || sda.Match["serial"] != "GIM256_2021" {
		t.Fatalf("sda header wrong: %+v", sda)
	}
	if sda.Partitions[0].Number != 1 {
		t.Fatalf("sda1 number: %+v", sda.Partitions[0])
	}

	if len(res.NICs) != 2 {
		t.Fatalf("lo must be skipped; nics = %d", len(res.NICs))
	}
	if !res.NICs[0].LinkUp || res.NICs[0].MAC != "aa:bb:cc:dd:ee:01" {
		t.Fatalf("eno1 facts: %+v", res.NICs[0])
	}
	if res.NICs[1].LinkUp {
		t.Fatalf("eno2 operstate DOWN must not be link_up: %+v", res.NICs[1])
	}
}

func TestCollectClassifiesRunFailures(t *testing.T) {
	cases := []struct {
		err  error
		want string
	}{
		{errors.New("ssh: unable to authenticate: password"), "CREDENTIAL_AUTH_FAILED"},
		{errors.New("dial tcp 10.0.0.5:22: connect: connection refused"), "NETWORK_UNREACHABLE"},
		{errors.New("dial tcp: i/o timeout"), "NETWORK_UNREACHABLE"},
		{errors.New("something odd"), "NETWORK_UNREACHABLE"}, // explicit failure, never a hang
	}
	for _, tc := range cases {
		c := &Collector{Runner: fakeRunner{err: tc.err}}
		_, err := c.Collect(context.Background(), "10.0.0.5", Credentials{})
		e, ok := err.(*Error)
		if !ok {
			t.Fatalf("%v: want *Error, got %T", tc.err, err)
		}
		if e.Code != tc.want {
			t.Fatalf("%v: code = %s, want %s", tc.err, e.Code, tc.want)
		}
	}
}

func TestCollectParseFailureIsExplicit(t *testing.T) {
	c := &Collector{Runner: fakeRunner{out: []byte("garbage without markers")}}
	_, err := c.Collect(context.Background(), "10.0.0.5", Credentials{})
	e, ok := err.(*Error)
	if !ok || e.Code != "LAYOUT_PARSE_FAILED" {
		t.Fatalf("want LAYOUT_PARSE_FAILED, got %v", err)
	}
}

// partitionNumber covers the naming schemes seen in the wild.
func TestPartitionNumber(t *testing.T) {
	cases := map[[2]string]int{
		{"nvme0n1p3", "nvme0n1"}: 3,
		{"sda2", "sda"}:          2,
		{"md0p1", "md0"}:         1,
		{"mmcblk0p1", "mmcblk0"}: 1,
	}
	for in, want := range cases {
		if got := partitionNumber(in[0], in[1]); got != want {
			t.Fatalf("partitionNumber(%v) = %d, want %d", in, got, want)
		}
	}
}

// The script must stay read-only: guard against someone adding a writing
// command later (docs/05-inventory.md §3: 不向目标机写入任何文件).
func TestScriptIsReadOnly(t *testing.T) {
	banned := []string{"mkfs", "dd ", "parted", "wipefs", "shred", "> /", "echo >", "tee ", "rm ", "mount ", "umount "}
	lower := strings.ToLower(scriptOf())
	for _, b := range banned {
		if strings.Contains(lower, b) {
			t.Fatalf("script contains forbidden write-ish token %q", b)
		}
	}
}

func scriptOf() string { return Script }

// The ENV section flags a running installer environment — verify_ready's
// guard against verifying against anaconda/subiquity/d-i instead of the
// freshly installed system (real-hardware false positive).
func TestCollectInstallerEnvMarker(t *testing.T) {
	// absence (recorded fixtures / older outputs) reads as a regular system
	res, err := parse(fixture)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if res.InstallerEnv {
		t.Errorf("fixture without ENV section must not read as installer env")
	}

	// an installer marker flips the flag without disturbing the layout
	installer := fixture + `===ENV===
/run/anaconda
`
	res, err = parse(installer)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !res.InstallerEnv {
		t.Errorf("anaconda marker must read as installer env")
	}
	if len(res.Layout.Disks) == 0 {
		t.Errorf("installer-env session must still carry the layout")
	}
}

// The BOND section carries the live bond state (post-install health check,
// docs/06-install-pipeline.md §4). Absence (recorded fixtures predating it)
// reads as "facts unavailable" (nil) — NOT as "no bonds"; a present-but-empty
// section is authoritative: the machine has no bonds.
func TestCollectBonds(t *testing.T) {
	// absence: nil, distinguishable from empty
	res, err := parse(fixture)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if res.Bonds != nil {
		t.Errorf("fixture without BOND section must yield nil Bonds, got %v", res.Bonds)
	}

	bonded := fixture + `===BOND===
bond0|mode=802.3ad 4|miimon=100|slaves=eno1 eno2|active=eno1
` + `===ENV===
/run/anaconda
`
	res, err = parse(bonded)
	if err != nil {
		t.Fatalf("parse bonded fixture: %v", err)
	}
	if len(res.Bonds) != 1 {
		t.Fatalf("one bond line expected, got %+v", res.Bonds)
	}
	b := res.Bonds[0]
	if b.Name != "bond0" || b.Mode != "802.3ad" || b.MIImon != 100 {
		t.Errorf("bond facts wrong: %+v", b)
	}
	if len(b.Slaves) != 2 || b.Slaves[0] != "eno1" || b.Slaves[1] != "eno2" || b.Active != "eno1" {
		t.Errorf("bond members/active wrong: %+v", b)
	}
	if !res.InstallerEnv {
		t.Errorf("ENV after BOND must still parse")
	}

	// present but empty: a real machine without bonds
	empty := fixture + `===BOND===
`
	res, err = parse(empty)
	if err != nil {
		t.Fatalf("parse empty-bond fixture: %v", err)
	}
	if res.Bonds == nil || len(res.Bonds) != 0 {
		t.Errorf("empty BOND section must read as no bonds (non-nil), got %v", res.Bonds)
	}
}

// The LVM section carries the live volume group view (docs/04-install-spec.md
// §5.1): post-install health check AND the keep:vg binding source. Absence
// (recorded fixtures, or a target without lvm2) reads as "facts unavailable"
// (nil) — NOT as "no volume groups"; vgs-with-no-groups reads as authoritative
// empty. LV mount facts join from the lsblk tree (type=lvm nodes).
func TestCollectLvm(t *testing.T) {
	// absence: nil, distinguishable from empty
	res, err := parse(fixture)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if res.Lvm != nil || res.Layout.Lvm != nil {
		t.Errorf("fixture without LVM section must yield nil Lvm facts, got %+v", res.Lvm)
	}

	lvmLSBLK := `{
  "blockdevices": [
    {"name": "sda", "path": "/dev/sda", "size": 960197124096, "type": "disk", "serial": "SER-A",
     "children": [
       {"name": "sda1", "path": "/dev/sda1", "size": 1073741824, "type": "part"},
       {"name": "sda2", "path": "/dev/sda2", "size": 959123392512, "type": "part",
        "children": [
          {"name": "vg0-root", "path": "/dev/mapper/vg0-root", "size": 21474836480,
           "type": "lvm", "fstype": "ext4", "mountpoint": "/"}
        ]}
     ]}
  ]
}
`
	full := lvmLSBLK + `===BLKID===
===LINK===
[{"link":"eth0"}]
===PARTS===
/sys/block/sda/sda2/start 1048576
===BOND===
===LVM===
---VGS---
vg0,1,107263365120B
---PVS---
/dev/sda2,vg0,107263365120B
---LVS---
vg0,root,21474836480B,/dev/dm-0
vg0,swap,4294967296B,/dev/dm-1
===ENV===
`
	res, err = parse(full)
	if err != nil {
		t.Fatalf("parse lvm fixture: %v", err)
	}
	if res.Lvm == nil || len(res.Lvm) != 1 {
		t.Fatalf("one volume group expected, got %+v", res.Lvm)
	}
	if res.Layout.Lvm == nil {
		t.Errorf("layout must carry the LVM facts (snapshot persistence)")
	}
	vg := res.Lvm[0]
	if vg.Name != "vg0" || vg.PVCount != 1 || vg.SizeBytes != 107263365120 {
		t.Errorf("vg facts wrong: %+v", vg)
	}
	if len(vg.PVs) != 1 || vg.PVs[0].Name != "sda2" || vg.PVs[0].SizeBytes != 107263365120 {
		t.Errorf("pv facts wrong: %+v", vg.PVs)
	}
	if len(vg.LVs) != 2 {
		t.Fatalf("two LVs expected, got %+v", vg.LVs)
	}
	root := vg.LVs[0]
	if root.Name != "root" || root.DMPath != "/dev/dm-0" {
		t.Errorf("root lv facts wrong: %+v", root)
	}
	if root.Mount != "/" || root.FSType != "ext4" {
		t.Errorf("lsblk join (mount/fstype) wrong: %+v", root)
	}
	if vg.LVs[1].Mount != "" || vg.LVs[1].Name != "swap" {
		t.Errorf("unmounted LV must carry no mount fact: %+v", vg.LVs[1])
	}

	// lvm2 present but no volume groups: authoritative empty (non-nil)
	empty := lvmLSBLK + `===BLKID===
===LINK===
[]
===PARTS===
===LVM===
---VGS---
---PVS---
---LVS---
`
	res, err = parse(empty)
	if err != nil {
		t.Fatalf("parse empty-lvm fixture: %v", err)
	}
	if res.Lvm == nil || len(res.Lvm) != 0 {
		t.Errorf("empty LVM section must read as no volume groups (non-nil), got %v", res.Lvm)
	}

	// lvm2 missing: the script emits no sub-markers — facts unavailable
	missing := lvmLSBLK + `===BLKID===
===LINK===
[]
===PARTS===
===LVM===
===ENV===
/run/anaconda
`
	res, err = parse(missing)
	if err != nil {
		t.Fatalf("parse lvm2-missing fixture: %v", err)
	}
	if res.Lvm != nil {
		t.Errorf("LVM section without sub-markers must read as facts unavailable, got %+v", res.Lvm)
	}
}
