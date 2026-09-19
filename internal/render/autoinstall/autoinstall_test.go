package autoinstall

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/3th1nk/mammoth/internal/bmc"
	"github.com/3th1nk/mammoth/internal/render"
)

// fetchUser decodes the emitted user-data JSON (cloud-init accepts JSON
// user-data; the golden assertions read it structurally).
func fetchUser(t *testing.T, answers []render.AnswerFile) (meta string, ud map[string]any) {
	t.Helper()
	for _, a := range answers {
		switch a.Name {
		case "meta-data":
			meta = a.Content
		case "user-data":
			raw := strings.TrimPrefix(a.Content, "#cloud-config\n")
			if err := json.Unmarshal([]byte(raw), &ud); err != nil {
				t.Fatalf("user-data json: %v\n%s", err, a.Content)
			}
		}
	}
	if ud == nil {
		t.Fatal("autoinstall.yaml missing")
	}
	return meta, ud
}

// The autoinstall dialect golden test: seed structure, netplan bond by MAC
// (the batch-stable selector), curtin storage for the wipe path, and the
// completion callback (docs/06-install-pipeline.md §5 matrix: partial).
func TestRenderAutoinstallWipeAndBond(t *testing.T) {
	d := New("ubuntu22")
	in := render.InstallInputs{
		TaskToken:     "tok9",
		MachineID:     "mch_u",
		Hostname:      "node-u1",
		ImageSource:   "https://mirror.example/ubuntu-22.04.iso",
		RootPassword:  "uRoot-pw",
		SSHPublicKeys: []string{"ssh-ed25519 AAA u@m"},
		BootDrive:     "nvme0n1",
		AnswerBaseURL: "https://m/render/tok9",
		CompleteURL:   "https://m/render/tok9/complete",
		DriftCheck:    false,
		Disks: []render.ResolvedDisk{
			{Device: "nvme0n1", Wipe: true, Partitions: []render.ResolvedPartition{
				{Mount: "/boot/efi", FS: "vfat", SizeMB: 512, Flags: []string{"esp"}},
				{Mount: "/", FS: "xfs", Grow: true},
			}},
		},
		Network: []render.NetworkEntry{
			{
				Bond: &render.NetBond{
					SlavesMACs: []string{"AA:BB:CC:DD:EE:01", "AA:BB:CC:DD:EE:02"},
					Mode:       "802.3ad",
					Params:     map[string]string{"miimon": "100"},
				},
				Addresses:   []string{"172.16.1.11/24"},
				Routes:      []render.NetRoute{{To: "default", Via: "172.16.1.1"}},
				Nameservers: []string{"10.0.0.53"},
			},
		},
		Scripts: []render.ScriptEntry{
			{Stage: "post_install", Inline: "echo u-done"},
		},
	}
	answers, boot, err := d.RenderAnswers(in, render.MachineView{
		ID: "mch_u", Hardware: &bmc.HardwareView{Disks: []bmc.DiskView{
			{Name: "nvme0n1", SizeBytes: 1920383410176},
		}},
	})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if len(answers) != 2 {
		t.Fatalf("nocloud seed = meta-data + user-data, got %+v", answers)
	}
	meta, ud := fetchUser(t, answers)
	_ = meta // nocloud meta-data must exist but carries no content

	auto, _ := ud["autoinstall"].(map[string]any)
	if auto == nil {
		t.Fatal("autoinstall section missing")
	}

	// access: root password + ssh keys are set during install (late-commands)
	// — anything left to first-boot cloud-init silently no-ops because the
	// booted system cannot read the seed (real-hardware lesson).
	if ud["chpasswd"] != nil {
		t.Errorf("chpasswd must not ride in user-data anymore (first-boot cannot read it)")
	}
	ssh, _ := auto["ssh"].(map[string]any)
	if _, ok := ssh["authorized-keys"]; ok {
		t.Errorf("authorized-keys must move to late-commands")
	}
	if auto["shutdown"] != "reboot" {
		t.Errorf("shutdown: reboot missing")
	}

	// netplan: bond slaves matched by MAC address natively
	net := auto["network"].(map[string]any)
	bonds := net["bonds"].(map[string]any)
	bond0 := bonds["bond0"].(map[string]any)
	ifaces := bond0["interfaces"].([]any)
	if len(ifaces) != 2 {
		t.Fatalf("bond slaves wrong: %v", ifaces)
	}
	eths := net["ethernets"].(map[string]any)
	for _, id := range ifaces {
		e := eths[id.(string)].(map[string]any)
		mac := e["match"].(map[string]any)["macaddress"]
		if mac != "aa:bb:cc:dd:ee:01" && mac != "aa:bb:cc:dd:ee:02" {
			t.Errorf("slave %v matches wrong mac %v", id, mac)
		}
	}
	if bond0["gateway4"] != "172.16.1.1" {
		t.Errorf("bond gateway wrong: %v", bond0["gateway4"])
	}

	// curtin storage: disk→partition→format→mount for the wiped disk
	st := auto["storage"].(map[string]any)
	config := st["config"].([]any)
	joined := ""
	for _, c := range config {
		b, _ := json.Marshal(c)
		joined += string(b) + "\n"
	}
	for _, want := range []string{
		`"path":"/dev/nvme0n1"`, `"ptable":"gpt"`, `"wipe":"superblock"`,
		`"fstype":"fat32"`, `"path":"/boot/efi"`, `"path":"/"`, `"grub_device":true`,
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("curtin storage missing %q", want)
		}
	}
	if strings.Contains(joined, "sda") {
		t.Errorf("no sda in this spec, storage leaked: %s", joined)
	}

	// completion callback + user script + hostname + access provisioning
	late := strings.Join(toStrSlice(auto["late-commands"]), "\n")
	if !strings.Contains(late, "https://m/render/tok9/complete") {
		t.Errorf("completion callback missing")
	}
	if !strings.Contains(late, "python3") {
		t.Errorf("completion callback must use python3 (curl absent in subiquity env)")
	}
	// The root password travels as a $6$ crypt (chpasswd -e): subiquity
	// stores identity.password verbatim, so a plain string there would be
	// an untypeable password.
	if !strings.Contains(late, `chpasswd -e`) {
		t.Errorf("root password provisioning (crypt) missing:\n%s", late)
	}
	if strings.Contains(late, "uRoot-pw") {
		t.Errorf("PLAIN root password leaked into the seed:\n%s", late)
	}
	if !strings.Contains(late, "$6$") {
		t.Errorf("no crypt hash in late-commands:\n%s", late)
	}
	if !strings.Contains(late, "PermitRootLogin yes") {
		t.Errorf("PermitRootLogin provisioning missing")
	}
	if !strings.Contains(late, "ssh-ed25519 AAA u@m") {
		t.Errorf("ssh key provisioning missing")
	}
	if !strings.Contains(late, "echo u-done") {
		t.Errorf("user post script missing")
	}
	if !strings.Contains(late, "node-u1") {
		t.Errorf("hostname missing")
	}
	// Symmetric with the debian post-install: a provisioned system must
	// never keep the pool's offline-apt tolerance (defense in depth — the
	// casper carrier stages no pool today, so this is a no-op there).
	if !strings.Contains(late, "rm -f /target/etc/apt/apt.conf.d/99mammoth-offline") {
		t.Errorf("offline-apt tolerance cleanup missing:\n%s", late)
	}

	if boot.KernelArgs != "autoinstall ds=nocloud-net;s=file:///cdrom/" {
		t.Errorf("boot params wrong: %q", boot.KernelArgs)
	}
}

// BIOS + GPT: a declared biosgrub partition renders as a raw bios_grub
// partition (curtin refuses bootloader install without an explicit one).
// /boot/efi without an explicit esp flag must be normalized: curtin types
// the ESP by flag (unlike anaconda, which infers it from the mountpoint) —
// a bare /boot/efi fails subiquity's "needed bootloader partition" check
// (real-hardware 2288H).
func TestRenderAutoESP(t *testing.T) {
	d := New("ubuntu22")
	in := render.InstallInputs{
		AnswerBaseURL: "https://m/render/t", CompleteURL: "https://m/render/t/complete",
		ImageSource: "https://mirror.example/ubuntu-22.04.iso",
		Disks: []render.ResolvedDisk{{Device: "vda", Wipe: true, SizeBytes: 214748364800,
			Partitions: []render.ResolvedPartition{
				{Mount: "/boot/efi", FS: "vfat", SizeMB: 512},
				{Mount: "/", FS: "ext4", Grow: true},
			}}},
	}
	user, _, err := d.RenderAnswers(in, render.MachineView{ID: "mch_x"})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	var ud string
	for _, a := range user {
		if a.Name == "user-data" {
			ud = a.Content
		}
	}
	if !strings.Contains(ud, `"flag": "boot"`) || !strings.Contains(ud, `"grub_device": true`) {
		t.Errorf("bare /boot/efi must render as a flagged ESP with grub_device")
	}
}

func TestRenderBiosGrubPartition(t *testing.T) {
	in := render.InstallInputs{
		AnswerBaseURL: "u", CompleteURL: "c", ImageSource: "i",
		Disks: []render.ResolvedDisk{
			{Device: "sda", Wipe: true, Partitions: []render.ResolvedPartition{
				{FS: "fat32", SizeMB: 1, Flags: []string{"biosgrub"}},
				{Mount: "/boot/efi", FS: "vfat", SizeMB: 512, Flags: []string{"esp"}},
				{Mount: "/", FS: "ext4", Grow: true}}},
		},
	}
	_, ud, err := func() ([]render.AnswerFile, map[string]any, error) {
		answers, _, err := New("ubuntu22").RenderAnswers(in, render.MachineView{})
		if err != nil {
			t.Fatalf("render: %v", err)
		}
		_, u := fetchUser(t, answers)
		return nil, u, nil
	}()
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	st, _ := json.Marshal(ud["autoinstall"].(map[string]any)["storage"])
	if !strings.Contains(string(st), `"flag":"bios_grub"`) {
		t.Errorf("bios_grub partition missing from curtin config: %s", st)
	}
}

// keep: disk on a partial distro: the disk is absent from the curtin config.
func TestRenderKeepDiskPartial(t *testing.T) {
	d := New("ubuntu22")
	in := render.InstallInputs{
		AnswerBaseURL: "https://m/render/t", CompleteURL: "https://m/render/t/complete",
		ImageSource: "i",
		Disks: []render.ResolvedDisk{
			{Device: "nvme0n1", Wipe: true, Partitions: []render.ResolvedPartition{
				{Mount: "/", FS: "xfs", Grow: true}}},
			{Device: "sdb", KeepDisk: true},
		},
	}
	answers, _, err := d.RenderAnswers(in, render.MachineView{})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	_, ud := fetchUser(t, answers)
	b, _ := json.Marshal(ud["autoinstall"].(map[string]any)["storage"])
	if strings.Contains(string(b), "sdb") {
		t.Errorf("keep:disk device must be absent from curtin config: %s", b)
	}
}

// keep: partitions on a partial distro is rejected at render (defense in
// depth behind the submit gate).
func TestRenderRejectsKeepPartitions(t *testing.T) {
	d := New("ubuntu22")
	in := render.InstallInputs{
		AnswerBaseURL: "u", CompleteURL: "c", ImageSource: "i",
		Disks: []render.ResolvedDisk{
			{Device: "sda", Baseline: []render.BaselinePartition{
				{Device: "sda1", Number: 1, StartBytes: 1048576, SizeBytes: 536870912},
			}, Partitions: []render.ResolvedPartition{
				{Mount: "/data", Preserve: true, Number: 1, OnPart: "sda1"},
			}},
		},
	}
	_, _, err := d.RenderAnswers(in, render.MachineView{})
	if err == nil || !strings.Contains(err.Error(), "not supported") {
		t.Fatalf("want keep:partitions rejection, got %v", err)
	}
}

func toStrSlice(v any) []string {
	out := []string{}
	if arr, ok := v.([]any); ok {
		for _, x := range arr {
			if s, ok := x.(string); ok {
				out = append(out, s)
			}
		}
	}
	return out
}

// The PXE variant: casper mounts the live root over NFS from the unpacked
// tree (no whole-ISO-into-RAM), and the nocloud seed rides HTTP. Static-net
// declarations are rejected — netplan cannot apply before the NFS root is up.
func TestRenderNetbootCasperArgs(t *testing.T) {
	d := New("ubuntu22")
	base := render.InstallInputs{
		TaskToken:     "toku",
		MachineID:     "mch_u",
		Hostname:      "node-u1",
		ImageSource:   "https://mirror.example/ubuntu-22.04.5-live-server-amd64.iso",
		RootPassword:  "uRoot-pw",
		BootDrive:     "sda",
		AnswerBaseURL: "http://10.0.0.1:8080/render/toku",
		CompleteURL:   "http://10.0.0.1:8080/render/toku/complete",
		Disks: []render.ResolvedDisk{
			{Device: "sda", Wipe: true, Partitions: []render.ResolvedPartition{
				{Mount: "/boot/efi", FS: "vfat", SizeMB: 512, Flags: []string{"esp"}},
				{Mount: "/", FS: "ext4", SizeMB: 20480},
			}},
		},
	}
	in := base
	in.Netboot = &render.NetbootInputs{
		PoolURL:    "http://10.0.0.1:8080/netboot/files/toku",
		NFSRootURL: "10.0.0.1:/export/netboot/toku/iso",
	}
	in.Network = []render.NetworkEntry{{
		Match: &render.NetMatch{MAC: "02:00:00:00:00:00"},
	}}
	answers, boot, err := d.RenderAnswers(in, render.MachineView{})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if boot.NetbootKernelArgs == "" ||
		!strings.Contains(boot.NetbootKernelArgs, "ds=nocloud-net;s=http://10.0.0.1:8080/render/toku/") ||
		!strings.Contains(boot.NetbootKernelArgs, "boot=casper netboot=nfs nfsroot=10.0.0.1:/export/netboot/toku/iso") ||
		!strings.Contains(boot.NetbootKernelArgs, "nfsopts=tcp,v3") ||
		!strings.Contains(boot.NetbootKernelArgs, "BOOTIF=01-02-00-00-00-00-00") {
		t.Errorf("netboot args missing casper/nfsroot/seed/tcp/BOOTIF: %q", boot.NetbootKernelArgs)
	}

	// A DHCP-only spec (the PXE-legal form) carries no network declaration —
	// the BOOTIF MAC then comes from the machine's NIC inventory.
	in.Network = nil
	_, boot2, err := d.RenderAnswers(in, render.MachineView{
		Hardware: &bmc.HardwareView{NICs: []bmc.NICView{
			{Name: "eno1", MAC: "02:00:00:00:00:00"},
			{Name: "eno2", MAC: "02:00:00:00:00:01"},
		}},
	})
	if err != nil {
		t.Fatalf("render dhcp-only: %v", err)
	}
	if !strings.Contains(boot2.NetbootKernelArgs, "BOOTIF=01-02-00-00-00-00-00") ||
		!strings.Contains(boot2.NetbootKernelArgs, " ip=dhcp ") {
		t.Errorf("dhcp-only render missing BOOTIF/dhcp: %q", boot2.NetbootKernelArgs)
	}

	// Precedence: the SPEC declaration is user intent and beats the pool
	// reservation.
	in.Network = []render.NetworkEntry{{
		Match:     &render.NetMatch{MAC: "02:00:00:00:00:00"},
		Addresses: []string{"198.51.100.50/24"},
		Routes:    []render.NetRoute{{To: "0.0.0.0/0", Via: "198.51.100.1"}},
	}}
	in.Netboot.StaticIP = "198.51.100.186"
	in.Netboot.StaticRouter = "198.51.100.240"
	in.Netboot.StaticMask = "255.255.255.0"
	_, bootP, err := d.RenderAnswers(in, render.MachineView{
		Hardware: &bmc.HardwareView{NICs: []bmc.NICView{{Name: "eno1", MAC: "02:00:00:00:00:00"}}},
	})
	if err != nil {
		t.Fatalf("render precedence: %v", err)
	}
	if !strings.Contains(bootP.NetbootKernelArgs, "ip=198.51.100.50::198.51.100.1:255.255.255.0:::off") {
		t.Errorf("spec static must beat the reservation:\n%s", bootP.NetbootKernelArgs)
	}
	// Reservation-only (dhcp-only spec): the reserved address drives both the
	// initramfs and (via the missing netplan) stays the machine's address.
	in.Network = nil
	_, boot3, err := d.RenderAnswers(in, render.MachineView{
		Hardware: &bmc.HardwareView{NICs: []bmc.NICView{{Name: "eno1", MAC: "02:00:00:00:00:00"}}},
	})
	if err != nil {
		t.Fatalf("render reserved: %v", err)
	}
	for _, want := range []string{
		"ip=198.51.100.186::198.51.100.240:255.255.255.0:::off",
		"BOOTIF=01-02-00-00-00-00-00",
		"nfsopts=tcp,v3",
	} {
		if !strings.Contains(boot3.NetbootKernelArgs, want) {
			t.Errorf("reserved render missing %q: %q", want, boot3.NetbootKernelArgs)
		}
	}
	if strings.Contains(boot3.NetbootKernelArgs, "ip=dhcp") {
		t.Errorf("reserved render still asks for dhcp: %q", boot3.NetbootKernelArgs)
	}
	// The seed files stay identical to the ISO path (ds= is an absolute URL).
	var found bool
	for _, a := range answers {
		if a.Name == "user-data" && strings.Contains(a.Content, "autoinstall") {
			found = true
		}
	}
	if !found {
		t.Errorf("user-data missing from netboot render: %+v", answers)
	}

	// Static network + PXE translates to static ip= kernel arguments —
	// the site-DHCP-safe shape: mammoth never becomes the address authority.
	// The declaration lands on the TARGET via a late-command netplan write
	// (subiquity otherwise inherits the installer environment's addressing);
	// pinned by TestRenderAutoinstallPXETargetNetplan.
	static := base
	static.Netboot = &render.NetbootInputs{NFSRootURL: "10.0.0.1:/export/netboot/toku/iso"}
	static.Network = []render.NetworkEntry{{
		Match:     &render.NetMatch{MAC: "aa:bb:cc:dd:ee:02"},
		Addresses: []string{"172.16.1.12/24"},
		Routes:    []render.NetRoute{{To: "0.0.0.0/0", Via: "172.16.1.1"}},
	}}
	_, bootS, err := d.RenderAnswers(static, render.MachineView{})
	if err != nil {
		t.Fatalf("static-net PXE render: %v", err)
	}
	for _, want := range []string{
		"ip=172.16.1.12::172.16.1.1:255.255.255.0:::off",
		"BOOTIF=01-aa-bb-cc-dd-ee-02",
	} {
		if !strings.Contains(bootS.NetbootKernelArgs, want) {
			t.Errorf("static-net PXE missing %q: %q", want, bootS.NetbootKernelArgs)
		}
	}
}

// The netboot carrier declarations: casper from the ISO + NFS tree source.
func TestNetbootDeclarationsUbuntu(t *testing.T) {
	d := New("ubuntu22")
	if c, p := render.NetbootInstallOf(d); c != render.NetbootCarrierISO || p != render.NetbootPoolNFS {
		t.Errorf("carrier/pool = %q/%q, want iso/nfs_tree", c, p)
	}
	if d.PXESupport() != render.SupportFull {
		t.Errorf("ubuntu22 PXE support must be full")
	}
}

// PXE without an NFS media base is rejected — casper's nfsroot would be
// empty and the live root could never mount (qemu verification finding).
func TestRenderNetbootRejectsEmptyNFSRoot(t *testing.T) {
	in := baseNetbootInputs()
	in.Netboot = &render.NetbootInputs{PoolURL: "http://10.0.0.2/netboot/files/toku"} // no NFSRootURL
	if _, _, err := New("ubuntu22").RenderAnswers(in, render.MachineView{}); err == nil {
		t.Fatal("empty NFSRootURL must be rejected")
	}
}

func baseNetbootInputs() render.InstallInputs {
	return render.InstallInputs{
		TaskToken: "toku", MachineID: "mch_u", Hostname: "node-u1",
		ImageSource: "https://mirror.example/u.iso", RootPassword: "pw",
		AnswerBaseURL: "http://10.0.2.2/render/toku",
		CompleteURL:   "http://10.0.2.2/render/toku/complete",
		Disks: []render.ResolvedDisk{{Device: "sda", Wipe: true, Partitions: []render.ResolvedPartition{
			{Mount: "/boot/efi", FS: "vfat", SizeMB: 512, Flags: []string{"esp"}},
			{Mount: "/", FS: "ext4", SizeMB: 8192},
		}}},
	}
}

// A controller-named volume without a serial (stale snapshot: the in-band
// probe predates serial collection) must resolve to the kernel name from
// the machine's in-band view by size — curtin's path form matches nothing
// on "LogicalDrive0" (2288H: "matched no disk").
func TestStorageResolvesControllerVolumeBySize(t *testing.T) {
	d := New("ubuntu22")
	in := render.InstallInputs{
		TaskToken:     "tokr",
		MachineID:     "mch_r",
		Hostname:      "node-r1",
		ImageSource:   "https://mirror.example/ubuntu-22.04.5-live-server-amd64.iso",
		RootPassword:  "rRoot-pw",
		BootDrive:     "LogicalDrive0",
		AnswerBaseURL: "http://10.0.0.1:8080/render/tokr",
		CompleteURL:   "http://10.0.0.1:8080/render/tokr/complete",
		Raid: []render.ResolvedRaid{{
			Name: "LogicalDrive0", Mode: "hardware", BoundDevice: "LogicalDrive0",
			SizeBytes: 3999999721472,
			Partitions: []render.ResolvedPartition{
				{Mount: "/boot/efi", FS: "vfat", SizeMB: 512, Flags: []string{"esp"}},
				{Mount: "/", FS: "ext4", Grow: true},
			},
		}},
	}
	// The snapshot's size differs slightly from the Redfish-reported volume
	// size (rounding between views) — the 1% band still resolves it.
	m := render.MachineView{Hardware: &bmc.HardwareView{Disks: []bmc.DiskView{
		{Name: "sda", SizeBytes: 4000225165312},
	}}}
	answers, _, err := d.RenderAnswers(in, m)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	var ud, resolver string
	for _, a := range answers {
		switch a.Name {
		case "user-data":
			ud = a.Content
		case "run/mammoth/resolve-disk.sh":
			resolver = a.Content
		}
	}
	// The kernel name is unknowable at render time (the snapshot carries the
	// same controller view), so the path stays a placeholder and the
	// on-machine resolver patches it — keyed by the controller name with the
	// render-side size hint.
	if !strings.Contains(ud, `"path": "/dev/LogicalDrive0"`) {
		t.Errorf("placeholder path missing:\n%s", ud)
	}
	if resolver == "" {
		t.Fatal("resolve-disk.sh not rendered for the controller-named volume")
	}
	if !strings.Contains(resolver, `"LogicalDrive0": 3999999721472`) ||
		!strings.Contains(resolver, `/autoinstall.yaml`) {
		t.Errorf("resolver missing size hint or config path:\n%s", resolver)
	}
	if !strings.Contains(ud, "resolve-disk.sh") {
		t.Errorf("early-command for the resolver missing:\n%s", ud)
	}
}

// PXE + static spec: the target's netplan is what subiquity/curtin generate
// from the installer environment — in pool-armed installs that inheritance
// carried the pool lease instead of the declared address (22.04-crypt round,
// 2026-09-19: installed .212, spec said .211). A late-command must pin the
// declared network into the target: drop the generated netplan files, write
// the spec verbatim. DHCP-only specs and virtual-media installs have no
// declared address to defend and stay untouched.
func TestRenderAutoinstallPXETargetNetplan(t *testing.T) {
	d := New("ubuntu22")
	in := baseNetbootInputs()
	in.Netboot = &render.NetbootInputs{NFSRootURL: "10.0.0.1:/export/netboot/tokn/iso"}
	in.Network = []render.NetworkEntry{{
		Match:     &render.NetMatch{MAC: "aa:bb:cc:dd:ee:09"},
		Addresses: []string{"172.16.1.21/24"},
		Routes:    []render.NetRoute{{To: "default", Via: "172.16.1.1"}},
	}}
	answers, boot, err := d.RenderAnswers(in, render.MachineView{})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	_, ud := fetchUser(t, answers)
	auto := ud["autoinstall"].(map[string]any)
	late := strings.Join(toStrSlice(auto["late-commands"]), "\n")
	for _, want := range []string{
		"rm -f /target/etc/netplan/00-installer-config.yaml /target/etc/netplan/50-cloud-init.yaml",
		"cat > /target/etc/netplan/99-mammoth.yaml <<'MAMMOTH_NETPLAN'",
		"addresses:",
		"172.16.1.21/24",
		"gateway4: 172.16.1.1",
		"macaddress: aa:bb:cc:dd:ee:09",
		"MAMMOTH_NETPLAN\nchmod 600 /target/etc/netplan/99-mammoth.yaml",
	} {
		if !strings.Contains(late, want) {
			t.Errorf("target netplan enforcement missing %q:\n%s", want, late)
		}
	}
	if !strings.Contains(boot.NetbootKernelArgs, "ip=172.16.1.21::172.16.1.1:255.255.255.0:::off") {
		t.Errorf("static ip= argument wrong: %q", boot.NetbootKernelArgs)
	}

	// Virtual media: no installer ip= environment to leak — no target write.
	vm := in
	vm.Netboot = nil
	answersVM, _, err := d.RenderAnswers(vm, render.MachineView{})
	if err != nil {
		t.Fatalf("virtual-media render: %v", err)
	}
	_, udVM := fetchUser(t, answersVM)
	autoVM := udVM["autoinstall"].(map[string]any)
	if lateVM := strings.Join(toStrSlice(autoVM["late-commands"]), "\n"); strings.Contains(lateVM, "99-mammoth.yaml") {
		t.Errorf("virtual-media render must not write target netplan:\n%s", lateVM)
	}

	// DHCP-only spec over PXE: no declared address to defend — no write.
	dhcpOnly := in
	dhcpOnly.Network = []render.NetworkEntry{{Match: &render.NetMatch{MAC: "aa:bb:cc:dd:ee:09"}}}
	answersDH, _, err := d.RenderAnswers(dhcpOnly, render.MachineView{})
	if err != nil {
		t.Fatalf("dhcp render: %v", err)
	}
	_, udDH := fetchUser(t, answersDH)
	autoDH := udDH["autoinstall"].(map[string]any)
	if lateDH := strings.Join(toStrSlice(autoDH["late-commands"]), "\n"); strings.Contains(lateDH, "99-mammoth.yaml") {
		t.Errorf("dhcp-only spec must not write target netplan:\n%s", lateDH)
	}
}
