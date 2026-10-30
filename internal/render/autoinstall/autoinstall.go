// Package autoinstall implements the OSDriver for Ubuntu Server autoinstall
// (subiquity) — docs/06-install-pipeline.md §5.
// Mechanics: the installer fetches a nocloud seed (meta-data + user-data)
// from Mammoth via `ds=nocloud-net;s=<task-token URL>/`; network stanzas use
// netplan (cloud-init network-config v2) whose `match.macaddress` is the
// batch-stable selector natively — no pre-install resolution needed.
// Keep-partition support is partial (docs/06-install-pipeline.md §5 matrix):
// keep: disk works via curtin storage config; keep: partitions is rejected.
package autoinstall

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"strings"

	"github.com/3th1nk/mammoth/internal/render"
	"gopkg.in/yaml.v3"
)

// Driver is the ubuntu22 autoinstall driver.
type Driver struct {
	distro string
}

// New returns the driver for one distro name (the subiquity dialect family
// currently has a single member, "ubuntu22").
func New(distro string) *Driver { return &Driver{distro: distro} }

func (d *Driver) Distro() string {
	if d.distro == "" {
		return "ubuntu22"
	}
	return d.distro
}

// SupportedArchs is amd64-only: ubuntu live-server ships no arm64 media —
// the former table over-claimed arm64 (an arm64 ubuntu needs a different
// carrier and medium, not this driver).
func (d *Driver) SupportedArchs() []render.Arch { return []render.Arch{render.ArchAMD64} }

// Family reports the installer family for the support matrix
// (docs/06-install-pipeline.md §5).
func (d *Driver) Family() string { return "autoinstall" }

// KeepPartitionSupport: subiquity/curtin can keep a whole disk (skip it in
// the storage config) but block-level partition reuse needs curtin surgery —
// declared partial; keep: partitions is rejected at submit and at render.
func (d *Driver) KeepPartitionSupport() render.SupportLevel { return render.SupportPartial }

// PXESupport: full via the casper path — boot files extract from the ISO and
// the live root (squashfs) mounts over NFS from the unpacked tree, so the
// whole-ISO-into-RAM trap (docs/compat/distros.md, the 4 GB casper lesson)
// never triggers (docs/06-install-pipeline.md §3.3, §6).
func (d *Driver) PXESupport() render.SupportLevel { return render.SupportFull }

// NetbootCarrier: the casper kernel/initrd extract from the distro ISO.
func (d *Driver) NetbootCarrier() render.NetbootCarrier { return render.NetbootCarrierISO }

// NetbootPool: casper mounts its squashfs root from the unpacked tree over
// NFS (netboot=nfs) — subiquity then installs from that same live source.
func (d *Driver) NetbootPool() render.NetbootPool { return render.NetbootPoolNFS }

// networkIsDHCP reports whether every entry leaves addressing to DHCP — the
// only form usable before casper's NFS root is up (netplan applies later,
// inside the installer).
func networkIsDHCP(entries []render.NetworkEntry) bool {
	for _, e := range entries {
		if len(e.Addresses) > 0 || e.Bond != nil || e.VLAN != nil {
			return false
		}
	}
	return true
}

// RenderAnswers produces the nocloud seed files and boot parameters.
func (d *Driver) RenderAnswers(in render.InstallInputs, m render.MachineView) ([]render.AnswerFile, render.BootParams, error) {
	if in.AnswerBaseURL == "" || in.CompleteURL == "" {
		return nil, render.BootParams{}, fmt.Errorf("%s: answer/completion URLs are required", d.distro)
	}
	primaryURL := strings.TrimSuffix(in.AnswerBaseURL, "/") + "/user-data"
	if in.ImageSource == "" {
		return nil, render.BootParams{}, fmt.Errorf("%s: image source is required", d.distro)
	}

	render.NormalizeESP(in.Disks)
	storage, dyn, err := d.storageConfig(in, m)
	if err != nil {
		return nil, render.BootParams{}, err
	}
	network, err := netplanConfig(in.Network)
	if err != nil {
		return nil, render.BootParams{}, err
	}
	if network == nil {
		// No declared network: render explicit DHCP on every inventory NIC
		// (matched by MAC — name-agnostic across udev renames, optional so
		// an unplugged port never stalls boot). Relying on subiquity's own
		// no-section default loses the net entirely when the INSTALLER env
		// had no working network (flaky site DHCP): subiquity then writes
		// no netplan and the installed system comes up offline (both LOMs
		// down, 2026-10-30 ubuntu24 vMedia round).
		network = defaultDhcpNetplan(m)
	}

	// late-commands: ALL post-install side effects happen here, in the
	// installer (real-hardware lesson: anything left to the target system's
	// first-boot cloud-init silently no-ops — its nocloud datasource cannot
	// read the seed once the system boots from disk and the CD mount point
	// is gone). That covers the root password, ssh keys and the completion
	// report. The completion POST uses python3 (present in the subiquity
	// environment); curl/wget are not guaranteed there.
	var late []string
	late = append(late, "echo mammoth-install-finished")
	// Defense-in-depth mirror of the debian post-install cleanup: the signed
	// pool's key deb (builder.StageNetbootPool, shared builder machinery)
	// ships an apt tolerance file for the INSTALLER's mirror verification,
	// and a provisioned system must never keep a permanently permissive apt
	// (an unsigned public mirror would pass silently). The casper carrier
	// stages no pool today, so this is a no-op there — cheap symmetry that
	// stays correct if the pool ever reaches a casper-carried target.
	// The apt tolerance cleanup rides curtin in-target like everything else:
	// subiquity runs late-commands inside a mount namespace where the target
	// root IS / — a literal /target path (the d-i habit) writes into the
	// installer environment and never reaches the disk (2026-10-28, 2288H:
	// the netplan block below silently no-oped on exactly this; the on-disk
	// /etc/netplan came up empty while every in-target command worked).
	late = append(late, "curtin in-target -- rm -f /etc/apt/apt.conf.d/99mammoth-offline")
	// The one-time password, crypt-hashed: subiquity stores identity.password
	// VERBATIM in /etc/shadow (a plain string there matches no login), and
	// chpasswd -e consumes the same hash for root (both users unreachable
	// after a green install, 2288H — rendered plain text was the cause).
	// The hash gets its own inner shQuote: the late-command text is itself a
	// sh -c script, so a bare $6$salt$hash would be variable-expanded by the
	// inner shell — the on-disk shadow ended up with the hash TAIL (70 chars,
	// "$6$"+salt eaten as expansions) and no root password login ever worked
	// (2288H 2026-10-28: mammoth login fine via identity, root broken).
	rootpwCrypt := ""
	if in.RootPassword != "" {
		salt := randomSalt()
		rootpwCrypt = cryptSHA512(in.RootPassword, salt)
		late = append(late, fmt.Sprintf("curtin in-target -- sh -c %s", quoteSh("echo root:"+quoteSh(rootpwCrypt)+" | chpasswd -e")))
	}
	late = append(late, fmt.Sprintf("curtin in-target -- sh -c %s", quoteSh("mkdir -p /etc/ssh/sshd_config.d && echo 'PermitRootLogin yes' > /etc/ssh/sshd_config.d/60-mammoth.conf")))
	// The target's own cloud-init generates the host keys on first boot and
	// prints the PRIVATE keys to the console by default — the autoinstall
	// ssh section does not reach it (the seed is gone by then), so the
	// tolerance rides a dropped-in cloud-init override instead.
	late = append(late, fmt.Sprintf("curtin in-target -- sh -c %s", quoteSh("mkdir -p /etc/cloud/cloud.cfg.d && printf 'ssh:\\n  emit_keys_to_console: false\\n' > /etc/cloud/cloud.cfg.d/99-mammoth.conf")))
	// Declared package repos (docs/04-install-spec.md §5.5): the deb822
	// entry lands in the target BEFORE the user scripts run (they may apt
	// install against it). The signing key, when declared, is fetched by
	// the installer environment's python3 (curl/wget are not guaranteed
	// there — the completion callback relies on python3 for the same
	// reason) to the target-side keyring the sources entry signs with.
	for _, r := range in.PackageSource {
		suite := r.Suite
		if suite == "" {
			suite = render.AptSuite(d.distro)
		}
		comps := r.Components
		if comps == "" {
			comps = "main"
		}
		signedBy := ""
		if r.GPGKey != "" {
			keyPath := fmt.Sprintf("/usr/share/keyrings/mammoth-%s.asc", r.Name)
			fetch := fmt.Sprintf("import urllib.request; urllib.request.urlretrieve(%q, '/target%s')", r.GPGKey, keyPath)
			late = append(late, "python3 -c "+quoteSh(fetch))
			signedBy = "Signed-By: " + keyPath + "\n"
		}
		body := fmt.Sprintf("Types: deb\nURIs: %s\nSuites: %s\nComponents: %s\n%s%s",
			r.URL, suite, comps, signedBy, trustedLine(r.GPGKey != ""))
		name := fmt.Sprintf("mammoth-%s.sources", r.Name)
		late = append(late, fmt.Sprintf("curtin in-target -- sh -c %s", quoteSh(
			"mkdir -p /etc/apt/sources.list.d && echo "+base64.StdEncoding.EncodeToString([]byte(body))+
				" | base64 -d > /etc/apt/sources.list.d/"+name)))
	}
	for _, k := range in.SSHPublicKeys {
		late = append(late, fmt.Sprintf("curtin in-target -- sh -c %s", quoteSh("mkdir -p /root/.ssh && echo "+quoteSh(k)+" >> /root/.ssh/authorized_keys && chmod 600 /root/.ssh/authorized_keys")))
	}
	// PXE real-hardware lesson (2026-09-19, the 22.04-crypt pool-armed
	// round): the target's netplan is whatever subiquity/curtin generates
	// from the INSTALLER environment — in pool-armed installs that
	// inheritance carried the pool lease (.212) instead of the declared
	// static address (.211): the spec-static-over-pool precedence diluted
	// on the target side. Enforce the contract in the target: drop the
	// generated netplan files and write the declared network verbatim —
	// a static spec is user intent, the address it declares is the
	// contract. Static specs only: a dhcp-only spec declares no address to
	// defend (the pool-reservation path owns that case), and virtual-media
	// installs have shown no dilution (no ip= environment to leak).
	// Ordered BEFORE the user scripts so a post_install probe reads the
	// written files back at the same installer stage (2026-10-28 round).
	if in.Netboot != nil {
		if _, ok := firstStaticNetwork(in.Network); ok {
			// curtin in-target + base64 (the package_source form): the
			// late-command environment mounts the target root at / in its
			// mount namespace, so heredoc writes to literal /target paths
			// never land on disk (2026-10-28, 2288H). base64 keeps the YAML
			// off the late-command quoting path entirely. The file gets the
			// top-level `network:` wrapper the autoinstall `network` section
			// deliberately lacks — subiquity consumes network-config v2
			// bare, but a netplan FILE under /etc/netplan needs the wrapper
			// (same-day read-back: bare "ethernets:" was rejected with
			// `unknown key 'ethernets'`, netplan generate died, and the
			// poisoned render took cloud-init's DHCP fallback down with it —
			// the system came up with NO network at all).
			wrapped, werr := yaml.Marshal(map[string]any{"network": network})
			if werr != nil {
				return nil, render.BootParams{}, fmt.Errorf("%s: target netplan: %w", d.distro, werr)
			}
			late = append(late,
				"curtin in-target -- sh -c "+quoteSh("rm -f /etc/netplan/00-installer-config.yaml /etc/netplan/50-cloud-init.yaml"),
				"curtin in-target -- sh -c "+quoteSh("mkdir -p /etc/netplan && echo "+
					base64.StdEncoding.EncodeToString(wrapped)+" | base64 -d > /etc/netplan/99-mammoth.yaml && chmod 600 /etc/netplan/99-mammoth.yaml"))
		}
	}
	for _, s := range in.Scripts {
		if s.Stage != "post_install" {
			continue
		}
		late = append(late, scriptLine(s))
	}
	// Completion callback with a patient retry loop: late-commands run in
	// the LIVE environment where networking may have come up late or not at
	// all (vMedia offline rounds) — a single 10s attempt turned finished
	// installs into subiquity "failures" (2026-10-30, ubuntu24
	// vMedia). 30×10s ≈ 5 min of patience; the callback is idempotent.
	late = append(late, fmt.Sprintf(`for i in $(seq 1 30); do python3 -c "import json,urllib.request;z=urllib.request.Request('%s',data=json.dumps({'status':'ok','detail':'autoinstall finished'}).encode(),headers={'Content-Type':'application/json'});urllib.request.urlopen(z,timeout=10)" && break; sleep 10; done`, in.CompleteURL))

	// pre_install scripts map to early-commands (installer environment).
	var early []string
	// Live-log forwarder, FIRST so the installer phase lands in the
	// engine's task log: subiquity does not consume the syslog= kernel
	// argument (d-i does) — without this the installer phase is invisible
	// in task logs (2026-10-30, the round that motivated the
	// syslog-port plumbing). journal → UDP BSD syslog via util-linux
	// logger (present in the live env); backgrounded — early-commands run
	// synchronously and a -f stream must not block the install. The
	// initramfs phase (casper) logs to serial only and is not covered.
	if host := render.SyslogHost(in.AnswerBaseURL); host != "" {
		port := in.SyslogPort
		if port == 0 {
			port = 514
		}
		early = append(early, fmt.Sprintf(
			"nohup sh -c 'journalctl -b -f -o short | logger -d -n %s -P %d -t mammoth-install' >/dev/null 2>&1 &",
			host, port))
	}
	// Controller-named volumes whose kernel name mammoth cannot know (the
	// snapshot itself carries the controller view): subiquity rewrites
	// nothing, so an early command resolves the device ON THE MACHINE by
	// size and patches /autoinstall.yaml before storage applies — the
	// autoinstall twin of the debian resolve-disk.sh early_command (the
	// kickstart dialect has the %pre equivalent). Real-hardware: curtin
	// "matched no disk" on /dev/LogicalDrive0 three times over.
	var resolveAnswers []render.AnswerFile
	if len(dyn) > 0 {
		resolveAnswers = []render.AnswerFile{{
			Name:    "run/mammoth/resolve-disk.sh",
			Content: resolveDiskScript(dyn),
		}}
		early = append(early, resolveDiskEarlyCommand(in))
	}
	for _, s := range in.Scripts {
		if s.Stage == "pre_install" {
			early = append(early, scriptLine(s))
		}
	}

	// subiquity refuses to start an unattended install without identity
	// ("neither identity nor user-data provided" — it checks BEFORE
	// curtin ever runs, so the late-command chpasswd alone is not enough;
	// real-hardware finding, PXE carrier). The password here is the same
	// one-time value the late command re-applies on the target.
	identity := map[string]any{
		"hostname": in.Hostname,
		"username": "mammoth",
		"password": rootpwCrypt,
	}
	auto := map[string]any{
		"version": 1,
		"locale":  "en_US.UTF-8",
		"keyboard": map[string]any{
			"layout": "us",
		},
		"identity": identity,
		"ssh": map[string]any{
			"install-server": true,
			"allow-pw":       true,
			// cloud-init prints the generated host PRIVATE keys to the local
			// console by default (headless-retrieval affordance) — a secret
			// This deployment does not need on the tty.
			"emit-keys-to-console": false,
		},
		"storage": storage,
		"late-commands": append([]string{
			// Write the file directly: hostnamectl needs a running systemd,
			// which the curtin chroot does not have (real-hardware lesson —
			// the command silently no-oped and the host came up as
			// localhost.localdomain). The path rides curtin in-target —
			// literal /target paths are inert in the late-command mount
			// namespace (see the netplan block above).
			fmt.Sprintf("curtin in-target -- sh -c %s", quoteSh("echo "+quoteSh(in.Hostname)+" > /etc/hostname")),
		}, late...),
		// Without this subiquity stalls after curtin instead of rebooting
		// into the freshly installed system (real-hardware lesson).
		"shutdown": "reboot",
	}
	if len(early) > 0 {
		auto["early-commands"] = early
	}
	if len(network) > 0 {
		auto["network"] = network
	}
	// The installer-env apt mirror (deployment-configured): subiquity checks
	// its apt mirror at install start; the distro default (archive.ubuntu.com)
	// is unreachable from mirrorless install L2s and the check gates the
	// install behind a dialog (real-hardware: 2288H, the 22.04.5 PXE round).
	if in.Netboot != nil && in.Netboot.AptMirror != "" {
		auto["apt"] = map[string]any{
			"primary": []map[string]any{{"arches": []string{"default"}, "uri": in.Netboot.AptMirror}},
		}
	}

	userData := map[string]any{
		"autoinstall": auto,
	}

	meta := "" // nocloud meta-data must exist (empty ok)
	userDataYAML, err := emitYAML(userData)
	if err != nil {
		return nil, render.BootParams{}, fmt.Errorf("%s: user-data: %w", d.distro, err)
	}

	// The answer files are BAKED into the rebuilt ISO root; the kernel
	// argument points the nocloud datasource at the CD mount (fully offline).
	// nocloud REQUIRES the meta-data file to exist alongside user-data.
	answers := []render.AnswerFile{
		{Name: "meta-data", Content: meta},
		{Name: "user-data", Content: userDataYAML},
	}
	answers = append(answers, resolveAnswers...)
	// The answer files are BAKED into the rebuilt ISO root; the kernel
	// argument points the nocloud-net datasource at the CD mount (file:// —
	// fully offline). The autoinstall network section (above) configures the
	// address for the install itself.
	// subiquity scans the boot medium's root for autoinstall.yaml when
	// `autoinstall` is on the kernel command line; the file:// seedfrom is
	// the documented offline-ISO form (casper mounts the boot medium at
	// /cdrom — cloud-init reads the seed from there, no networking).
	// syslog= forwards casper/subiquity logs to mammoth's sink (UDP 514 on
	// the machine-face host, or the deployment's sink port — rendered as
	// host:port when off 514) on BOTH carriers — the vMedia shape needs it
	// just as much (the offline seed proves nothing about log visibility).
	boot := render.BootParams{
		AnswerURL:           primaryURL,
		KernelArgs:          "autoinstall ds=nocloud-net;s=file:///cdrom/",
		InstallerAutoReboot: true, // shutdown: reboot
	}
	if host := render.SyslogHost(in.AnswerBaseURL); host != "" {
		boot.KernelArgs += " syslog=" + render.SyslogTarget(host, in.SyslogPort)
	}
	// PXE: casper mounts the pool tree at /cdrom and moves that mount into
	// the live root, so the tree-root seed files ARE visible — but the seed
	// cannot ride a file:///cdrom/ seedfrom here: cloud-init 24.2's
	// DataSourceNoCloudNet accepts only http(s)/ftp seedfrom
	// (supported_seed_starts — "Seed from file://... not supported"), and
	// the local ds=nocloud form proved equally inert on real hardware
	// (2288H: five INSTALL_TIMEOUT cycles across both markers with the
	// seed verified present at the tree root). The engine's HTTP answer
	// endpoint is the one channel the boot itself proves reachable — the
	// machine mounts its live root from this very host over NFS — so the
	// seed rides the answer URL (the canonical subiquity PXE recipe).
	// Topologies where the NFS media host and the engine differ AND the
	// machine cannot reach the engine over HTTP have no seed channel at
	// all: subiquity falls back to the interactive picker rather than
	// installing blind, which the INSTALL_TIMEOUT surfaces.
	if in.Netboot != nil {
		if in.Netboot.NFSRootURL == "" {
			return nil, render.BootParams{}, fmt.Errorf("%s: PXE installs need an NFS media base for the casper live root (set MAMMOTH_MEDIA_BASE_URI=nfs://<host>/<export> on the runner)", d.distro)
		}
		// Addressing precedence: spec static declaration (user intent — the
		// declared address is the contract, netplan below already carries it)
		// → pool reservation (system-chosen, dhcp-only specs) → DHCP. The
		// static forms feed the initramfs directly (the boot-time DHCP is
		// racy on real hardware — udev renames the NIC mid-ipconfig).
		ipArg := "ip=dhcp"
		if e, ok := firstStaticNetwork(in.Network); ok {
			addr := strings.SplitN(e.Addresses[0], "/", 2)[0]
			ipArg = fmt.Sprintf("ip=%s::%s:%s:::off",
				addr, e.Routes[0].Via, maskOfCIDR(e.Addresses[0]))
		} else if in.Netboot.StaticIP != "" {
			ipArg = fmt.Sprintf("ip=%s::%s:%s:::off",
				in.Netboot.StaticIP, in.Netboot.StaticRouter, in.Netboot.StaticMask)
		}
		// The seedfrom URL must carry the trailing slash: cloud-init
		// urlappend joins user-data/meta-data onto it verbatim. console=tty0
		// mirrors kernel output onto the VGA console (a KVM lab watching the
		// graphical console otherwise stares at the last firmware line while
		// the kernel panics or drops to initramfs on serial, 2026-10-29
		// lab finding); console=ttyS0 LAST keeps serial as /dev/console:
		// SOL (ipmitool sol activate) still carries the whole install —
		// kernel, cloud-init datasource decisions, subiquity — to the engine
		// operator even when no human sits at the VGA console (five
		// interactive-fallback rounds on the 2288H went undiagnosed without
		// this). Same shape as the agent/probe carriers.
		seedBase := strings.TrimSuffix(in.AnswerBaseURL, "/") + "/"
		// Console order: serial primary (SOL operator surface) unless the
		// deployment asked for the graphical console — labs/KVM debug the
		// installer through the VGA (the stall/failure lines face the
		// console the operator actually watches).
		consoleArgs := "console=tty0 console=ttyS0,115200n8"
		if in.InstallConsole == "vga" {
			consoleArgs = "console=ttyS0,115200n8 console=tty0"
		}
		boot.NetbootKernelArgs = fmt.Sprintf(
			"autoinstall ds=nocloud-net;s=%s %s %s boot=casper netboot=nfs nfsroot=%s nfsopts=tcp,v3",
			seedBase, ipArg, consoleArgs, in.Netboot.NFSRootURL)
		if host := render.SyslogHost(in.AnswerBaseURL); host != "" {
			boot.NetbootKernelArgs += " syslog=" + render.SyslogTarget(host, in.SyslogPort)
		}
		// BOOTIF pins the NIC casper's configure_networking configures: udev
		// renames interfaces mid-initramfs (2288H: enp1s0 → eno1 between
		// ipconfig's device scan and its DHCP), and ipconfig then times out
		// against the vanished name. pxelinux-style "01-<mac>" survives any
		// rename — the functions resolve the device by MAC, not by name.
		bootif := ""
		for _, n := range in.Network {
			if n.Match == nil || n.Match.MAC == "" {
				continue
			}
			bootif = "01-" + strings.ToLower(strings.NewReplacer(":", "-", ".", "-").Replace(n.Match.MAC))
			break
		}
		if bootif == "" && m.Hardware != nil {
			for _, n := range m.Hardware.NICs {
				if n.MAC == "" {
					continue
				}
				bootif = "01-" + strings.ToLower(strings.NewReplacer(":", "-", ".", "-").Replace(n.MAC))
				break
			}
		}
		if bootif != "" {
			boot.NetbootKernelArgs += " BOOTIF=" + bootif
		}
	}
	return answers, boot, nil
}

// firstStaticNetwork returns the first network entry carrying a static
// address with a default route — the spec-driven replacement for the pool
// reservation on deployments where a site DHCP owns addressing.
func firstStaticNetwork(entries []render.NetworkEntry) (render.NetworkEntry, bool) {
	for _, e := range entries {
		if len(e.Addresses) > 0 && strings.Contains(e.Addresses[0], "/") &&
			len(e.Routes) > 0 && (e.Routes[0].To == "default" || e.Routes[0].To == "0.0.0.0/0") {
			return e, true
		}
	}
	return render.NetworkEntry{}, false
}

// maskOfCIDR converts "a.b.c.d/p" into a dotted netmask.
func maskOfCIDR(cidr string) string {
	_, ipnet, err := net.ParseCIDR(cidr)
	if err != nil {
		return "255.255.255.0"
	}
	mask := net.IPMask(ipnet.Mask)
	if len(mask) != 4 {
		return "255.255.255.0"
	}
	return fmt.Sprintf("%d.%d.%d.%d", mask[0], mask[1], mask[2], mask[3])
}

// sizeWithinTolerance reports b within 1% of a (either may be 0/unknown —
// unknown only matches unknown). Redfish capacity reports vary slightly
// between refresh passes and views; the kickstart %pre and debian
// early_command resolvers use the same ±1% band.
func sizeWithinTolerance(a, b int64) bool {
	if a == 0 || b == 0 {
		return a == b
	}
	diff := a - b
	if diff < 0 {
		diff = -diff
	}
	return diff*100 <= a
}

// storageConfig builds the curtin storage config (autoinstall.storage).
// Wiped disks get the full disk→partition→format→mount chain; kept disks are
// simply absent (curtin never touches them) — the partial keep semantics.
func (d *Driver) storageConfig(in render.InstallInputs, m render.MachineView) (map[string]any, []dynDisk, error) {
	config := []map[string]any{}
	rendered := 0
	// Controller-named volumes the render side cannot resolve to a kernel
	// name (mammoth's snapshot carries the same controller view) — resolved
	// on the machine by resolve-disk.sh.
	var dyn []dynDisk

	// Hardware RAID volumes are install targets like disks — the bound
	// volume identifies by its SCSI serial (refreshed from the in-band
	// snapshot at configure_raid; the controller assigns it, Redfish does
	// not expose it).
	type diskTarget struct {
		id, device, serial string
		wipe               bool
		sizeBytes          int64
		partitions         []render.ResolvedPartition
	}
	targets := []diskTarget{}
	for _, r := range in.Raid {
		if r.Mode != "hardware" || r.BoundDevice == "" {
			continue
		}
		serial := ""
		if r.VolumeSerial != "" {
			serial = r.VolumeSerial // curtin matches the volume's SCSI serial
		} else if render.IsKernelDeviceName(r.BoundDevice) {
			serial = "" // kernel names take the path form
		}
		targets = append(targets, diskTarget{id: "raid-" + r.Name, device: r.BoundDevice,
			serial: serial, wipe: true, sizeBytes: r.SizeBytes, partitions: r.Partitions})
	}
	for _, disk := range in.Disks {
		if disk.KeepDisk {
			continue
		}
		if len(disk.Baseline) > 0 || len(disk.Remove) > 0 || hasPreserve(disk.Partitions) {
			return nil, nil, fmt.Errorf(
				"%s: keep: partitions is not supported on this distro (partial); submit without preserve", d.distro)
		}
		targets = append(targets, diskTarget{id: "disk-" + disk.Device, device: disk.Device,
			serial: disk.Serial, wipe: disk.Wipe, sizeBytes: diskSizeOf(m, disk.Device),
			partitions: disk.Partitions})
	}

	const gap = int64(1024 * 1024)
	for _, disk := range targets {
		if len(disk.partitions) == 0 {
			continue
		}
		if !disk.wipe {
			return nil, nil, fmt.Errorf("%s: disk %s must declare wipe or keep", d.distro, disk.device)
		}
		diskBytes := disk.sizeBytes

		// curtin identifies drives by serial (udev ID_SERIAL_SHORT) —
		// controller logical drives report their SCSI serial there, while
		// their kernel names are unstable. Path is the fallback for
		// serial-less (fake) disks.
		entry := map[string]any{
			"id":     disk.id,
			"type":   "disk",
			"ptable": "gpt",
			"wipe":   "superblock",
			"name":   "mammoth-" + disk.device,
		}
		if disk.serial != "" {
			entry["serial"] = disk.serial
		} else {
			// Kernel names take the path form. A controller-named volume
			// ("LogicalDrive0") is a placeholder only: the kernel name exists
			// solely on the machine — mammoth's snapshot carries the same
			// controller view, so render-side resolution is impossible (2288H:
			// "matched no disk" three times). The path is emitted as-is and
			// resolve-disk.sh patches it on the machine before storage applies.
			path := "/dev/" + disk.device
			if render.IsKernelDeviceName(disk.device) {
				// Snapshot cross-check: keep size drift visible (±1%, the band
				// every resolver in this codebase uses).
				if disk.sizeBytes > 0 && diskSizeOf(m, disk.device) > 0 &&
					!sizeWithinTolerance(diskSizeOf(m, disk.device), disk.sizeBytes) {
					return nil, nil, fmt.Errorf("%s: disk %s drifted from the snapshot size — re-probe the machine",
						d.distro, disk.device)
				}
			} else {
				dyn = append(dyn, dynDisk{name: disk.device, size: disk.sizeBytes})
			}
			entry["path"] = path
		}
		entry["grub_device"] = true
		rendered++
		config = append(config, entry)

		// Pre-compute grow sizes: rest takes the remainder minus a 1MiB gap
		// per partition (alignment headroom).
		var fixed int64
		growCount := 0
		for _, p := range disk.partitions {
			if p.Grow {
				growCount++
			} else {
				fixed += int64(p.SizeMB) * 1024 * 1024
			}
		}
		var restSize int64
		if growCount > 0 && diskBytes > 0 {
			restSize = diskBytes - fixed - gap*int64(len(disk.partitions))
			if restSize < gap {
				restSize = gap
			}
		}

		for i, p := range disk.partitions {
			num := i + 1
			partID := fmt.Sprintf("part-%s-%d", disk.id, num)
			part := map[string]any{
				"id":     partID,
				"type":   "partition",
				"device": disk.id,
				"number": num,
				"size":   fmt.Sprintf("%dM", p.SizeMB),
				"wipe":   "superblock",
			}
			// BIOS + GPT needs the 1MiB bios_grub partition (curtin refuses to
			// install grub without an explicit one); UEFI uses the esp partition
			// (mounted by the OS as /boot/efi). A layout may declare both.
			if hasFlag(p.Flags, "biosgrub") {
				part["flag"] = "bios_grub"
				config = append(config, part)
				continue // raw partition: no filesystem, no mount
			}
			if hasFlag(p.Flags, "esp") {
				// subiquity's bootloader check requires the ESP partition to
				// carry the boot flag AND be marked as the grub device — a
				// plain fat32/efi format without grub_device fails the
				// "needed bootloader partition" check.
				part["flag"] = "boot"
				part["grub_device"] = true
			}
			if p.Grow {
				part["size"] = restSize
			}
			config = append(config, part)

			fstype := p.FS
			if hasFlag(p.Flags, "esp") {
				fstype = "fat32"
			}
			fID := fmt.Sprintf("fmt-%s-%d", disk.id, num)
			config = append(config, map[string]any{
				"id":     fID,
				"type":   "format",
				"volume": partID,
				"fstype": fstype,
			})
			if p.Mount != "" && p.Mount != "swap" {
				config = append(config, map[string]any{
					"id":     fmt.Sprintf("mnt-%s-%d", disk.id, num),
					"type":   "mount",
					"path":   p.Mount,
					"device": fID,
				})
			}
		}
	}
	if rendered == 0 {
		return nil, nil, fmt.Errorf("%s: storage config is empty", d.distro)
	}
	return map[string]any{"version": 1, "config": config}, dyn, nil
}

// dynDisk is a storage target whose path carries a controller name — the
// kernel name exists only on the machine, so resolve-disk.sh rewrites it
// there (keyed by the controller name, size-hinted).
type dynDisk struct {
	name string
	size int64
}

// resolveDiskScript renders the on-machine resolver: patch every
// controller-named path in /autoinstall.yaml to the device lsblk finds by
// size (±1%; an unknown size falls back to the sole disk — the single-volume
// shape this dialect targets). Runs as an early command, before storage.
func resolveDiskScript(dyn []dynDisk) string {
	var b strings.Builder
	b.WriteString("#!/bin/sh\n")
	b.WriteString("# mammoth: resolve controller-named disk paths to kernel names, on the machine.\n")
	b.WriteString("python3 - <<'PY'\nimport re, subprocess, sys\n")
	b.WriteString(fmt.Sprintf("SIZES = %s\n", dynLiteral(dyn)))
	b.WriteString(`try:
    raw = open("/autoinstall.yaml").read()
except OSError:
    sys.exit(0)
devs = []
for ln in subprocess.run(["lsblk", "-dnb", "-o", "NAME,SIZE"],
                         capture_output=True, text=True).stdout.splitlines():
    n, _, s = ln.partition(" ")
    devs.append(("/dev/" + n, int(s)))
def pick(size):
    if size > 0:
        m = [p for p, sz in devs if abs(sz - size) * 100 <= size]
        if len(m) == 1:
            return m[0]
        return None
    return devs[0][0] if len(devs) == 1 else None
def repl(m):
    d = SIZES.get(m.group(2))
    if d is None:
        return m.group(0)
    dev = pick(d)
    if dev is None:
        sys.stderr.write("no unique device matches %s (%d bytes)\n" % (m.group(2), d))
        sys.exit(1)
    return m.group(1) + dev + m.group(3)
new = re.sub(r'("path"\s*:\s*"|path:\s*)/dev/([A-Za-z0-9_.\-]+)("?)', repl, raw)
open("/autoinstall.yaml", "w").write(new)
PY
`)
	return b.String()
}

// dynLiteral renders the name→size map as a Go-quoted python dict literal.
func dynLiteral(dyn []dynDisk) string {
	parts := make([]string, 0, len(dyn))
	for _, d := range dyn {
		parts = append(parts, fmt.Sprintf("%q: %d", d.name, d.size))
	}
	return "{" + strings.Join(parts, ", ") + "}"
}

// resolveDiskEarlyCommand fetches (netboot) or sources (ISO) the resolver.
func resolveDiskEarlyCommand(in render.InstallInputs) string {
	if in.Netboot != nil {
		return "wget -qO /tmp/mammoth-resolve-disk.sh " +
			strings.TrimSuffix(in.AnswerBaseURL, "/") + "/run/mammoth/resolve-disk.sh && sh /tmp/mammoth-resolve-disk.sh"
	}
	return "sh /cdrom/run/mammoth/resolve-disk.sh"
}

// bondParamNetplan maps the canonical kernel parameter names onto the
// netplan spellings — the vocabulary the ubuntu chain actually consumes
// (subiquity's model is netplan-only, and netplan's parse.c bond table
// keys on the netplan names; kernel names are not in it and stay inert).
// Keys ABSENT from this map are kernel-only params netplan cannot express
// (use_carrier, ad_actor_*, tlb_dynamic_lb) — the ubuntu dialect rejects
// them at render (docs/04-install-spec.md §5.3 discipline).
var bondParamNetplan = map[string]string{
	"miimon":             "mii-monitor-interval",
	"lacp_rate":          "lacp-rate",
	"xmit_hash_policy":   "transmit-hash-policy",
	"fail_over_mac":      "fail-over-mac-policy",
	"primary_reselect":   "primary-reselect-policy",
	"arp_interval":       "arp-interval",
	"arp_ip_target":      "arp-ip-targets",
	"arp_validate":       "arp-validate",
	"arp_all_targets":    "arp-all-targets",
	"up":                 "up-delay",
	"down":               "down-delay",
	"ad_select":          "ad-select",
	"all_slaves_active":  "all-slaves-active",
	"min_links":          "min-links",
	"num_grat_arp":       "gratuitous-arp",
	"packets_per_slave":  "packets-per-slave",
	"learn_pkt_interval": "learn-packet-interval",
	"resend_igmp":        "resend-igmp",
	"primary":            "primary",
}

func netplanConfig(entries []render.NetworkEntry) (map[string]any, error) {
	if len(entries) == 0 {
		return nil, nil
	}
	out := map[string]any{"version": 2}
	ethernets := map[string]any{}
	bonds := map[string]any{}
	applyAddrs := func(m map[string]any, e render.NetworkEntry) {
		if len(e.Addresses) > 0 {
			m["addresses"] = e.Addresses
		}
		var routes []map[string]any
		for _, r := range e.Routes {
			if r.To == "default" {
				// Default gateway rides the routes list, not the deprecated
				// gateway4 key: netplan has warned on gateway4 since 0.105
				// and newer releases (26.04's netplan) reject it outright —
				// a rejected netplan file poisons the whole render and takes
				// the interface down (2288H 2026-10-29, the windows tree
				// plumbing's same-day lesson generalized: never emit a
				// config key the newest netplan refuses).
				routes = append(routes, map[string]any{"to": "default", "via": r.Via})
				continue
			}
			routes = append(routes, map[string]any{"to": r.To, "via": r.Via})
		}
		if len(routes) > 0 {
			m["routes"] = routes
		}
		if len(e.Nameservers) > 0 {
			ns := map[string]any{"addresses": e.Nameservers}
			if len(e.Search) > 0 {
				ns["search"] = e.Search
			}
			m["nameservers"] = ns
		}
		if e.MTU > 0 {
			m["mtu"] = e.MTU
		}
	}

	// Bonds name by declaration order (bond0, bond1, …) — vlan.link references
	// that name, so the numbering must not shift when a plain interface sits
	// between two bonds.
	bondSeq := 0
	for i, e := range entries {
		switch {
		case e.Bond != nil:
			bond, err := e.Bond.Normalized()
			if err != nil {
				return nil, fmt.Errorf("network[%d]: %w", i, err)
			}
			var ids []string
			for _, mac := range bond.SlavesMACs {
				id := fmt.Sprintf("if-%s-%d", strings.ToLower(strings.ReplaceAll(mac, ":", "")), i)
				eth := map[string]any{
					"match": map[string]any{"macaddress": strings.ToLower(mac)},
				}
				if e.MTU > 0 {
					eth["mtu"] = e.MTU
				}
				ethernets[id] = eth
				ids = append(ids, id)
			}
			// Canonical kernel-name params translate to the netplan spellings
			// here — the outbound side of the spec-vocabulary fold
			// (NetBond.Normalized). Kernel-only params without a netplan
			// form reject rather than emit a key netplan will silently drop.
			params := map[string]any{
				"mode": bond.Mode,
			}
			for k, v := range bond.Params {
				np, ok := bondParamNetplan[k]
				if !ok {
					return nil, fmt.Errorf("network[%d]: bond param %q has no netplan form (kernel-only; the netplan-shaped network section cannot express it)", i, k)
				}
				if k == "arp_ip_target" {
					// netplan takes the target list as a YAML sequence.
					params[np] = strings.Fields(v)
					continue
				}
				params[np] = v
			}
			b := map[string]any{
				"interfaces": ids,
				"parameters": params,
			}
			applyAddrs(b, e)
			bonds[render.BondName(bondSeq)] = b
			bondSeq++
		case e.VLAN != nil:
			vlans := map[string]any{}
			v := map[string]any{"id": e.VLAN.ID, "link": e.VLAN.Link}
			applyAddrs(v, e)
			vlans[fmt.Sprintf("vlan%d", e.VLAN.ID)] = v
			out["vlans"] = vlans
		default:
			if e.Match == nil || (e.Match.MAC == "" && e.Match.Name == "") {
				continue // render-level guard: caller validated earlier
			}
			id := e.Match.Name
			eth := map[string]any{}
			if e.Match.MAC != "" {
				eth["match"] = map[string]any{"macaddress": strings.ToLower(e.Match.MAC)}
			}
			if e.SetName != "" {
				eth["set-name"] = e.SetName
			}
			if len(e.Addresses) == 0 {
				eth["dhcp4"] = true
			} else {
				eth["dhcp4"] = false
				applyAddrs(eth, e)
			}
			key := id
			if key == "" {
				key = fmt.Sprintf("if-%s", strings.ToLower(strings.ReplaceAll(e.Match.MAC, ":", "")))
			}
			ethernets[key] = eth
		}
	}
	if len(ethernets) > 0 {
		out["ethernets"] = ethernets
	}
	if len(bonds) > 0 {
		out["bonds"] = bonds
	}
	return out, nil
}

func scriptLine(s render.ScriptEntry) string {
	if s.Inline != "" {
		return "curtin in-target -- sh -c " + quoteSh(s.Inline)
	}
	if s.URL != "" {
		return "curtin in-target -- sh -c " + quoteSh("curl -fsS "+s.URL+" | sh")
	}
	return ""
}

func quoteSh(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
}

func hasFlag(flags []string, want string) bool {
	for _, f := range flags {
		if f == want {
			return true
		}
	}
	return false
}

func orDefault(v, def int) int {
	if v == 0 {
		return def
	}
	return v
}

// emitYAML renders the seed as JSON: cloud-init accepts JSON user-data
// (any JSON value after #cloud-config), so no YAML dependency is needed and
// the output is deterministic for golden tests.
func emitYAML(v map[string]any) (string, error) {
	b := &strings.Builder{}
	b.WriteString("#cloud-config\n")
	enc := json.NewEncoder(b)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return "", err
	}
	return b.String(), nil
}

func hasPreserve(parts []render.ResolvedPartition) bool {
	for _, p := range parts {
		if p.Preserve {
			return true
		}
	}
	return false
}

// diskSizeOf finds a hardware disk's capacity by inventory name.
func diskSizeOf(m render.MachineView, device string) int64 {
	if m.Hardware == nil {
		return 0
	}
	for _, hd := range m.Hardware.Disks {
		if hd.Name == device {
			return hd.SizeBytes
		}
	}
	return 0
}

// trustedLine marks an unsigned declared repo trusted — the operator
// declared it deliberately; the renderer records that in the sources entry.
func trustedLine(keyed bool) string {
	if keyed {
		return ""
	}
	return "Trusted: yes\n"
}

// defaultDhcpNetplan builds the no-declared-network fallback: DHCP4 on
// every inventory NIC, matched by MAC. nil when the machine view carries
// no hardware inventory (subiquity's own default then applies).
func defaultDhcpNetplan(m render.MachineView) map[string]any {
	if m.Hardware == nil || len(m.Hardware.NICs) == 0 {
		return nil
	}
	ethernets := map[string]any{}
	for i, n := range m.Hardware.NICs {
		if n.MAC == "" {
			continue
		}
		ethernets[fmt.Sprintf("nic%d", i)] = map[string]any{
			"match":    map[string]any{"macaddress": strings.ToLower(n.MAC)},
			"dhcp4":    true,
			"optional": true,
		}
	}
	if len(ethernets) == 0 {
		return nil
	}
	return map[string]any{"version": 2, "ethernets": ethernets}
}
