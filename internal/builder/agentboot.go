// Agent install media (docs/12-agent-initramfs.md): the mammoth agent is a
// minimal install runtime inside an alpine disk-less root — the same carrier
// the ramdisk probe uses (netboot tarball + apkovl overlay), with a much
// longer job: read the rendered plan (agent-plan.sh), partition the target
// disks, install the base system from the package pool, configure
// identity/network/bootloader, report completion, and reboot into the new
// system. No distro installer runs on the machine — the declarative Install
// Spec is consumed directly, which is the whole point of the pilot.
package builder

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
)

// AgentOverlayName is the overlay file name at the ISO root (the initramfs
// auto-applies *.apkovl.tar.gz found there); the netboot carrier writes the
// same content under the same name into the boot tree.
const AgentOverlayName = "mammoth.apkovl.tar.gz"

// AgentOverlay builds the agent's apkovl overlay for the boot media: the
// runtime script under etc/local.d/ plus the openrc hookup. The script is
// plan-independent (it fetches/reads the plan at runtime), so it is built
// once per carrier assembly, not per task render.
func AgentOverlay() (name string, data []byte, err error) {
	data, err = apkovlArchive(agentOverlayEntries(agentScript()))
	if err != nil {
		return "", nil, err
	}
	return AgentOverlayName, data, nil
}

// agentOverlayEntries mirrors probeOverlayEntries — same openrc mechanics,
// different payload script. With an overlay present the initramfs skips its
// default boot services; the marker restores them (hardware, modloop).
func agentOverlayEntries(script string) []cpioEntry {
	return []cpioEntry{
		{Name: "etc/.default_boot_services", Mode: 0o100644, Body: []byte{}},
		{Name: "etc/local.d/", Mode: 0o040755},
		{Name: "etc/local.d/mammoth-agent.start", Mode: 0o100755, Body: []byte(script)},
		{Name: "etc/runlevels/default/local", Mode: 0o120777, Link: "/etc/init.d/local"},
	}
}

// AgentNetbootOptions configure the agent's network boot payload.
type AgentNetbootOptions struct {
	// TarballPath is the alpine NETBOOT tarball (kernel/initrd/modloop),
	// already resolved through EnsureISO by the caller. Required — the
	// standard-ISO initramfs lacks the machine room's NIC drivers (probe
	// finding), and the agent needs the network for the plan and the pool.
	TarballPath string
	// ApksISOPath is the alpine distro ISO whose /apks package repository
	// is extracted into the boot tree — the memory root's AND the target's
	// package source.
	ApksISOPath string
	// DestDir is the per-task boot tree (MediaDir/netboot/<token>).
	DestDir string
	// XorrisoPath overrides the xorriso binary (default: PATH lookup).
	XorrisoPath string
	// Overlay is the agent apkovl content (builder.AgentOverlay), written
	// into the tree as agent.apkovl.tar.gz and fetched by URL via apkovl=.
	Overlay []byte
	// ModloopURL / ApksURL / OverlayURL are the URLs the caller renders into
	// the kernel arguments (the driver composes them from the same file
	// layout); they are recorded here only to keep the tree self-describing.
	ModloopURL string
	ApksURL    string
	OverlayURL string
}

// BuildAgentNetboot assembles the agent install payload as a network boot
// tree. Kernel arguments come from the agent driver's BootParams
// (NetbootKernelArgs) — the tree only publishes the files they reference.
func BuildAgentNetboot(ctx context.Context, opt AgentNetbootOptions) (BootTree, error) {
	if opt.TarballPath == "" {
		return BootTree{}, fmt.Errorf("builder: alpine netboot tarball is required")
	}
	if len(opt.Overlay) == 0 {
		return BootTree{}, fmt.Errorf("builder: agent overlay is required")
	}
	if err := os.MkdirAll(opt.DestDir, 0o755); err != nil {
		return BootTree{}, err
	}
	if err := extractTarFiles(ctx, opt.TarballPath, opt.DestDir, map[string]string{
		"boot/vmlinuz-lts":   "vmlinuz",
		"boot/initramfs-lts": "initrd.img",
		"boot/modloop-lts":   "modloop",
	}); err != nil {
		return BootTree{}, err
	}
	tree := BootTree{Dir: opt.DestDir, Kernel: "vmlinuz", Initrd: "initrd.img",
		Extra: map[string]string{"modloop": "modloop"}}
	if opt.ApksISOPath != "" {
		if err := extractIsoDir(ctx, opt.XorrisoPath, opt.ApksISOPath,
			"/apks", filepath.Join(opt.DestDir, "apks")); err != nil {
			return BootTree{}, fmt.Errorf("builder: apks repo extraction: %w", err)
		}
		tree.Extra["apks"] = "dir:apks"
	}
	if err := os.WriteFile(filepath.Join(tree.Dir, "agent.apkovl.tar.gz"), opt.Overlay, 0o644); err != nil {
		return BootTree{}, err
	}
	tree.Extra["apkovl"] = "agent.apkovl.tar.gz"
	return tree, nil
}

// agentScript is the busybox-sh install runtime. The plan's facts arrive as
// data declarations (mammoth_disk / mammoth_partition / mammoth_network /
// mammoth_script) collected into temp files while the plan sources — every
// quote lives in the shell plan, not in JSON parsing (busybox has none).
// Layout decisions the plan does not fix are made here at runtime, on the
// hardware: UEFI → GPT + ESP (auto-added when the spec declares none),
// BIOS → dos label + MBR grub.
func agentScript() string {
	return `#!/bin/sh
# mammoth agent — declarative install runtime (docs/12-agent-initramfs.md)
# openrc evals this script inside its service shell which runs with errexit —
# any failing command would kill the whole runtime silently (no report, no
# shell). This runtime handles its own errors: strip the -e and re-exec. The
# child keeps stderr on the console; MAMMOTH_AGENT_TRACE=1 adds set -x.
SCRIPT_VERSION="ab-v13"
case "$-" in *e*) sh +e "$0" "$@"; exit $? ;; esac
log "agent runtime $SCRIPT_VERSION starting"

PLAN=/tmp/mammoth-plan.sh
TARGET=/target
BASE="$(sed -n 's/.*mammoth_base=\([^ ]*\).*/\1/p' /proc/cmdline)"

log() { echo "[mammoth-agent] $*" > /dev/console; echo "[mammoth-agent] $*" >> /tmp/mammoth-log.txt 2>/dev/null; }

# ── plan collectors (the plan sources with these defined) ──────────────────
DISKS=/tmp/mammoth-disks
PARTS=/tmp/mammoth-parts
NETS=/tmp/mammoth-nets
SCRIPTS=/tmp/mammoth-scripts
: > "$DISKS"; : > "$PARTS"; : > "$NETS"; : > "$SCRIPTS"
mammoth_disk() { printf '%s|%s\n' "$1" "$2" >> "$DISKS"; }
mammoth_partition() { printf '%s|%s|%s|%s|%s\n' "$1" "$2" "$3" "$4" "$5" >> "$PARTS"; }
mammoth_network() { printf '%s|%s|%s|%s\n' "$1" "$2" "$3" "$4" >> "$NETS"; }
mammoth_script() { printf '%s|%s|%s|%s\n' "$1" "$2" "$3" "$4" >> "$SCRIPTS"; }

find_nic_by_mac() {
    want=$(echo "$1" | tr 'A-Z' 'a-z')
    [ -z "$want" ] && return 0
    for nic in /sys/class/net/*; do
        n="${nic##*/}"; [ "$n" = "lo" ] && continue
        have=$(cat "$nic/address" 2>/dev/null | tr 'A-Z' 'a-z')
        [ "$have" = "$want" ] && { echo "$n"; return 0; }
    done
}

first_phys_nic() {
    for nic in /sys/class/net/*; do
        n="${nic##*/}"; [ "$n" = "lo" ] && continue
        echo "$n"; return 0
    done
}

# ── network (runtime bring-up for the plan fetch and the callback) ─────────
bringup_network() {
    for nic in /sys/class/net/*; do
        n="${nic##*/}"; [ "$n" = "lo" ] && continue
        ip link set "$n" up 2>/dev/null
        if udhcpc -i "$n" -n -q -t 4 -T 2 >/dev/null 2>&1; then
            log "dhcp up on $n"; return 0
        fi
    done
    while IFS='|' read -r mac addrs gw dns; do
        [ "$addrs" != "-" ] && [ -n "$addrs" ] || continue
        n=$(find_nic_by_mac "$mac"); [ -z "$n" ] && n=$(first_phys_nic)
        [ -n "$n" ] || return 1
        ip addr add "$(echo "$addrs" | cut -d, -f1)" dev "$n" 2>/dev/null
        [ "$gw" != "-" ] && [ -n "$gw" ] && ip route replace default via "$gw" dev "$n" 2>/dev/null
        log "static up on $n"
        return 0
    done < "$NETS"
    return 1
}

report() { # report <ok|failed> <detail>
    DETAIL=$(printf '%s' "$2" | tr -d '"\\' | cut -c1-700)
    printf '{"status":"%s","detail":"%s"}' "$1" "$DETAIL" > /tmp/mammoth-report.json
    bringup_network || return 1
    for i in 1 2 3; do
        if wget -q -T 10 -O /dev/null --post-file=/tmp/mammoth-report.json "$MAMMOTH_COMPLETE_URL"; then
            log "completion reported ($1)"; return 0
        fi
        log "report attempt $i failed"; sleep 2
    done
    return 1
}

bail() { # bail <stage> <detail>
    log "FATAL ($1): $2"
    ERRTRAIL=$(tail -c 400 /tmp/mammoth-storage.err 2>/dev/null | tr '\n' ' ' | tr -d '"\\')
    LOGTRAIL=$(tail -c 400 /tmp/mammoth-log.txt 2>/dev/null | tr '\n' '|' | tr -d '"\\')
    TMPTYPE=$(df -h /tmp 2>/dev/null | tail -1 | cut -d" " -f1)
    report failed "$1: $2 [script: ${SCRIPT_VERSION:-unknown}] [err: ${ERRTRAIL:-none}] [log: ${LOGTRAIL:-none}] [tmpfs: ${TMPTYPE:-unknown}]"
    log "dropping to shell for diagnosis (BMC SOL)"
    exec /bin/sh
}

find_plan() {
    for d in /media/*/ ; do
        if [ -f "${d}agent-plan.sh" ]; then PLAN="${d}agent-plan.sh"; return 0; fi
    done
    if [ -n "$BASE" ]; then
        wget -q -T 10 -O /tmp/mammoth-plan.sh "$BASE/agent-plan.sh" && return 0
    fi
    return 1
}

bootstrap_tools() {
    # The agent bootstraps its own toolchain from the pool — the same repo
    # the base system installs from (boot media apks or alpine_repo= URL).
    apk add --no-cache sfdisk util-linux e2fsprogs dosfstools openssl >/dev/console 2>&1
    # filesystem modules for the target mounts (ext4 usually auto-loads via
    # modprobe, but the modloop lookup is not worth racing)
    modprobe ext4 >/dev/console 2>&1
    modprobe vfat >/dev/console 2>&1
}

run_stage() { # run_stage <stage>
    stage=$1
    [ -s "$SCRIPTS" ] || return 0
    rc=0
    while IFS='|' read -r s content url exits; do
        [ "$s" = "$stage" ] || continue
        sf="/tmp/mammoth-script-$stage.sh"
        if [ "$url" != "-" ] && [ -n "$url" ]; then
            wget -q -T 10 -O "$sf" "$url" || { log "fetch $url failed"; rc=1; continue; }
        elif [ "$content" != "-" ] && [ -n "$content" ]; then
            printf '%s' "$content" | base64 -d > "$sf" 2>/dev/null || { log "bad script encoding"; rc=1; continue; }
        else
            continue
        fi
        sh "$sf"; src_rc=$?
        allowed=0; [ "$src_rc" = 0 ] && allowed=1
        if [ "$exits" != "-" ] && [ -n "$exits" ]; then
            for e in $(echo "$exits" | tr ',' ' '); do
                [ "$src_rc" = "$e" ] && allowed=1
            done
        fi
        [ "$allowed" = 1 ] || { log "$stage script exit $src_rc not accepted"; rc=1; }
    done < "$SCRIPTS"
    return $rc
}

partnode() { # partition device name for disk+number (nvme/mmcblk use pN)
    case "$1" in
        *[!0-9]) echo "$1$2" ;;
        *) echo "${1}p$2" ;;
    esac
}

has_esp() { # does <disk> declare a partition carrying the esp flag?
    awk -F'|' -v d="$1" '$1==d && $5 ~ /(^|,)esp(,|$)/ {f=1} END{ if (f) exit 0; exit 1 }' "$PARTS"
}

mk_table() { # mk_table <disk> <uefi> — writes the sfdisk input and the
             # final partition layout (/tmp/mammoth-final-<disk>)
    disk=$1; uefi=$2
    tbl=/tmp/mammoth-table-$disk
    final=/tmp/mammoth-final-$disk
    : > "$final"
    if [ "$uefi" = 1 ]; then
        echo "label: gpt" > "$tbl"
    else
        echo "label: dos" > "$tbl"
    fi
    n=1
    # UEFI + boot drive without a declared ESP: the bootloader has nowhere
    # to live — prepend a 300MiB EFI System Partition (declared ESPs win).
    autoesp=0
    if [ "$uefi" = 1 ] && [ "$disk" = "$MAMMOTH_BOOT_DRIVE" ] && ! has_esp "$disk"; then
        echo "/dev/$(partnode "$disk" "$n"): size=614400, type=C12A7328-F81F-11D2-BA4B-00A0C93EC93B" >> "$tbl"
        printf '%s|%d|%s|%s|%s\n' "$disk" "$n" "/boot/efi" "vfat" "esp" >> "$final"
        autoesp=1; n=$((n+1))
        log "$disk: auto ESP added for UEFI (spec declares none)"
    fi
    while IFS='|' read -r d mount fs size flags; do
        [ "$d" = "$disk" ] || continue
        if [ "$uefi" = 1 ]; then
            case "$fs" in
                vfat) type="C12A7328-F81F-11D2-BA4B-00A0C93EC93B" ;;
                swap) type="0657FD6D-A4AB-43C4-84E5-0933C84B4F4F" ;;
                *)    type="0FC63DAF-8483-4772-8E79-3D69D8477DE4" ;;
            esac
        else
            case "$fs" in
                vfat) type="0c" ;;
                swap) type="82" ;;
                *)    type="83" ;;
            esac
        fi
        if [ "$size" = "-" ] || [ -z "$size" ]; then
            echo "/dev/$(partnode "$disk" "$n"): type=$type" >> "$tbl"
        else
            echo "/dev/$(partnode "$disk" "$n"): size=$((size * 2048)), type=$type" >> "$tbl"
        fi
        printf '%s|%d|%s|%s|%s\n' "$disk" "$n" "$mount" "$fs" "$flags" >> "$final"
        n=$((n+1))
    done < "$PARTS"
    log "$disk: partitioning ($([ "$uefi" = 1 ] && echo gpt || echo dos))"
    wipefs -a "/dev/$disk" >>/tmp/mammoth-storage.err 2>&1
    # transient-tolerant: an unclean prior shutdown leaves the controller
    # briefly busy / the partition re-read failing — retry instead of
    # dying silently (2288H: storage flaky only after unclean resets)
    attempt=1
    while : ; do
        if sfdisk --force "/dev/$disk" < "$tbl" >>/tmp/mammoth-storage.err 2>&1; then
            break
        fi
        if [ $attempt -ge 3 ]; then
            log "storage: sfdisk $disk FAILED after $attempt attempts"
            return 1
        fi
        log "storage: sfdisk attempt $attempt failed, retrying"
        attempt=$((attempt+1))
        sleep 3
        mdev -s 2>/dev/null
    done
}

resolve_disks() { # controller-named volumes have no kernel node — resolve
                  # by size (±1%, min 64MiB tolerance), kickstart-%pre 同款
    resolved=/tmp/mammoth-disks-kernel
    : > "$resolved"
    while IFS='|' read -r dsk size; do
        [ -n "$dsk" ] || continue
        node="$dsk"
        if [ "$size" != "-" ] && [ -n "$size" ]; then
            found=""
            bestdiff=0
            while read -r nm sz; do
                [ "$nm" != "$dsk" ] || continue
                diff=$((sz - size)); [ $diff -lt 0 ] && diff=$((-diff))
                if [ -z "$found" ] || [ $diff -lt $bestdiff ]; then found=$nm; bestdiff=$diff; fi
            done <<MAMMOTH_LSBLK
$(lsblk -dnb -o NAME,SIZE,TYPE 2>/dev/null | awk '$3=="disk"{print $1, $2}')
MAMMOTH_LSBLK
            tol=$((size / 100)); [ $tol -lt 67108864 ] && tol=67108864
            if [ -n "$found" ] && [ $bestdiff -le $tol ]; then
                log "storage: resolved $dsk -> /dev/$found (off by $bestdiff bytes)"
                node="$found"
                # rewrite the partition collector too — its lines are keyed by
                # the controller name and mk_table filters them by the
                # resolved name
                awk -F'|' -v d="$dsk" -v n="$node" 'BEGIN{OFS="|"} $1==d{$1=n} {print}' "$PARTS" > "$PARTS.new" \
                    && mv "$PARTS.new" "$PARTS"
            else
                log "storage: cannot resolve $dsk (size $size, closest $found off by $bestdiff)"
                return 1
            fi
        fi
        echo "$node" >> "$resolved"
    done < "$DISKS"
    DISKS="$resolved"
}

apply_storage() {
    resolve_disks || return 1
    log "storage: partitioning $(cat "$DISKS" | tr '\n' ' ')"
    mkdir -p "$TARGET"
    uefi=0; [ -d /sys/firmware/efi ] && uefi=1
    while read -r disk; do
        [ -n "$disk" ] || continue
        mk_table "$disk" "$uefi" >>/tmp/mammoth-storage.err 2>&1 || { log "storage: mk_table FAILED"; return 1; }
    done < "$DISKS"
    mdev -s 2>/dev/null
    log "storage: table written"
    # format + mount: root first (fstab needs its mountpoint), then every
    # other declared partition in table order
    ROOT_NODE=""
    for disk in $(cat "$DISKS"); do
        while IFS='|' read -r d num mount fs flags; do
            [ "$d" = "$disk" ] || continue
            [ "$mount" = "/" ] || continue
            [ "$fs" = "swap" ] && continue
            node="/dev/$(partnode "$d" "$num")"
            log "storage: mkfs $node"
            mkfs.ext4 -F "$node" >>/tmp/mammoth-storage.err 2>&1 || { log "storage: mkfs FAILED"; return 1; }
            mkdir -p "$TARGET$mount"
            log "storage: mount $node"
            mount "$node" "$TARGET$mount" >>/tmp/mammoth-storage.err 2>&1 || { log "storage: mount FAILED"; return 1; }
            # the fresh root is EMPTY — the alpine-baselayout skeleton (/etc,
            # /root, /var, …) only appears once apk installs alpine-base; the
            # fstab and key copies below need /etc to exist right now
            mkdir -p "$TARGET/etc" "$TARGET/root" "$TARGET/tmp" "$TARGET/dev" \
                     "$TARGET/proc" "$TARGET/sys" "$TARGET/media" "$TARGET/var" "$TARGET/boot"
            chmod 1777 "$TARGET/tmp" 2>/dev/null
            uuid=$(blkid -s UUID -o value "$node")
            echo "UUID=$uuid $mount $fs defaults 0 1" >> "$TARGET/etc/fstab"
            ROOT_NODE="$node"
        done < "/tmp/mammoth-final-$disk"
    done
    [ -n "$ROOT_NODE" ] || { log "no root filesystem mounted"; return 1; }
    : > "$TARGET/etc/fstab" 2>/dev/null
    echo "UUID=$(blkid -s UUID -o value "$ROOT_NODE") / ext4 defaults 0 1" > "$TARGET/etc/fstab"
    ESP_MOUNT=""
    for disk in $(cat "$DISKS"); do
        while IFS='|' read -r d num mount fs flags; do
            [ "$d" = "$disk" ] || continue
            node="/dev/$(partnode "$d" "$num")"
            if [ "$fs" = "swap" ]; then
                mkswap "$node" >>/tmp/mammoth-storage.err 2>&1 || { log "storage: mkswap $node FAILED"; return 1; }
                uuid=$(blkid -s UUID -o value "$node")
                echo "UUID=$uuid none swap sw 0 0" >> "$TARGET/etc/fstab"
                continue
            fi
            [ "$mount" = "/" ] && continue
            case "$fs" in
                ext4) mkfs.ext4 -F "$node" >>/tmp/mammoth-storage.err 2>&1 || { log "storage: mkfs.ext4 $node FAILED"; return 1; } ;;
                vfat) mkfs.vfat -F 32 "$node" >>/tmp/mammoth-storage.err 2>&1 || { log "storage: mkfs.vfat $node FAILED"; return 1; } ;;
                *) log "unsupported fs $fs"; return 1 ;;
            esac
            mkdir -p "$TARGET$mount"
            mount "$node" "$TARGET$mount" || return 1
            case "$flags" in *esp*) ESP_MOUNT="$mount" ;; esac
            echo "UUID=$(blkid -s UUID -o value "$node") $mount $fs defaults 0 2" >> "$TARGET/etc/fstab"
        done < "/tmp/mammoth-final-$disk"
    done
    log "storage applied (root $(blkid -s UUID -o value "$ROOT_NODE"))"
    return 0
}

install_base() {
    # keys from the memory root (alpine-keys came with the disk-less base);
    # repo list as the initramfs set it (boot media or alpine_repo= URL)
    mkdir -p "$TARGET/etc/apk/keys"
    cp -a /etc/apk/keys/. "$TARGET/etc/apk/keys/" 2>/dev/null
    cp /etc/apk/repositories "$TARGET/etc/apk/repositories" 2>/dev/null
    if [ ! -s /etc/apk/repositories ]; then
        for d in /media/*/apks; do
            [ -d "$d" ] && echo "$d" | tee -a /etc/apk/repositories "$TARGET/etc/apk/repositories" >/dev/null
        done
    fi
    apk add --root "$TARGET" --initdb --no-cache $MAMMOTH_PACKAGES >/dev/console 2>&1 || return 1
    log "base system installed from pool: $MAMMOTH_PACKAGES"
}

write_network_config() {
    mkdir -p "$TARGET/etc/network"
    dns_declared=0
    {
        echo "auto lo"
        echo "iface lo inet loopback"
        echo
        if [ -s "$NETS" ]; then
            while IFS='|' read -r mac addrs gw dns; do
                n=$(find_nic_by_mac "$mac"); [ -z "$n" ] && continue
                if [ "$addrs" = "-" ] || [ -z "$addrs" ]; then
                    echo "auto $n"; echo "iface $n inet dhcp"; echo
                else
                    echo "auto $n"; echo "iface $n inet static"
                    echo "  address $(echo "$addrs" | cut -d, -f1)"
                    [ "$gw" != "-" ] && [ -n "$gw" ] && echo "  gateway $gw"
                    echo
                    if [ "$dns" != "-" ] && [ -n "$dns" ]; then
                        for d in $(echo "$dns" | tr ',' ' '); do echo "nameserver $d"; done > "$TARGET/etc/resolv.conf"
                        dns_declared=1
                    fi
                fi
            done < "$NETS"
        else
            for nic in /sys/class/net/*; do
                n="${nic##*/}"; [ "$n" = "lo" ] && continue
                echo "auto $n"; echo "iface $n inet dhcp"; echo
            done
        fi
    } > "$TARGET/etc/network/interfaces"
    [ "$dns_declared" = 1 ] || cp /etc/resolv.conf "$TARGET/etc/resolv.conf" 2>/dev/null
    return 0
}

configure_target() {
    log "config: hostname/hosts"
    echo "$MAMMOTH_HOSTNAME" > "$TARGET/etc/hostname"
    printf '127.0.0.1\tlocalhost %s\n::1\tlocalhost\n' "$MAMMOTH_HOSTNAME" > "$TARGET/etc/hosts"
    # root password: sha512-crypt (busybox cryptpw; openssl passwd -6 as the
    # fallback), written into shadow — a plaintext shadow entry would make
    # the machine unloginnable (ubuntu22 real-hardware finding, mirrored).
    log "config: root password hash"
    hash=$(busybox cryptpw -m sha512 "$MAMMOTH_ROOT_PASSWORD" 2>/dev/null)
    [ -n "$hash" ] || hash=$(openssl passwd -6 "$MAMMOTH_ROOT_PASSWORD" 2>/dev/null)
    [ -n "$hash" ] || return 1
    awk -F: -v h="$hash" 'BEGIN{OFS=":"} $1=="root"{$2=h} {print}' "$TARGET/etc/shadow" > "$TARGET/etc/shadow.new" \
        && mv "$TARGET/etc/shadow.new" "$TARGET/etc/shadow" || return 1
    log "config: ssh keys + permit root login"
    if [ -n "$MAMMOTH_SSH_KEYS" ]; then
        mkdir -p "$TARGET/root/.ssh" && chmod 700 "$TARGET/root/.ssh"
        printf '%s\n' "$MAMMOTH_SSH_KEYS" > "$TARGET/root/.ssh/authorized_keys"
        chmod 600 "$TARGET/root/.ssh/authorized_keys"
    fi
    printf 'PermitRootLogin yes\n' >> "$TARGET/etc/ssh/sshd_config"
    log "config: network"
    write_network_config || return 1
    log "config: openrc runlevels (chroot)"
    # openrc runlevels: a bare apk --root install enables nothing
    for s in devfs dmesg mdev hwdrivers; do chroot "$TARGET" rc-update add "$s" sysinit >/dev/console 2>&1; done
    for s in hwclock modules sysctl hostname bootmisc syslog networking; do chroot "$TARGET" rc-update add "$s" boot >/dev/console 2>&1; done
    chroot "$TARGET" rc-update add sshd default >/dev/console 2>&1
    for s in killprocs mount-ro savecache; do chroot "$TARGET" rc-update add "$s" shutdown >/dev/console 2>&1; done
    log "target configured (hostname $MAMMOTH_HOSTNAME)"
}

install_bootloader() {
    uefi=0; [ -d /sys/firmware/efi ] && uefi=1
    # grub runs from the AGENT env against the target root (--root-directory):
    # no chroot — a chroot cannot reach the local apks repo (bind mounts do
    # not recurse into /media's submounts) while the agent env's own repo
    # already works. grub's device probing uses the live /dev /proc /sys.
    if [ "$uefi" = 1 ]; then
        pkgs="grub grub-efi"; [ -n "$ESP_MOUNT" ] || ESP_MOUNT=/boot/efi
    else
        pkgs="grub grub-bios"
    fi
    apk add --no-cache $pkgs >/dev/console 2>&1 || return 1
    if [ "$uefi" = 1 ]; then
        # --removable writes the fallback path (EFI/BOOT/BOOTX64.EFI) and
        # --no-nvram skips efivarfs: the machine face restores boot order,
        # and firmware without a boot entry still finds the fallback.
        grub-install --root-directory="$TARGET" --removable --no-nvram \
            --efi-directory="$TARGET$ESP_MOUNT" --boot-directory="$TARGET/boot" >/dev/console 2>&1 || return 1
    else
        grub-install --root-directory="$TARGET" --no-floppy "/dev/$MAMMOTH_BOOT_DRIVE" >/dev/console 2>&1 || return 1
    fi
    kflavor=$(ls "$TARGET"/boot/vmlinuz-* 2>/dev/null | head -1 | sed 's|.*/vmlinuz-||')
    [ -n "$kflavor" ] || return 1
    rootuuid=$(blkid -s UUID -o value "$ROOT_NODE")
    [ -n "$rootuuid" ] || return 1
    mkdir -p "$TARGET/boot/grub"
    cat > "$TARGET/boot/grub/grub.cfg" <<EOF
set default=0
set timeout=1
menuentry 'mammoth' {
    linux /boot/vmlinuz-$kflavor root=UUID=$rootuuid modules=sd-mod,usb-storage,ext4 quiet
    initrd /boot/initramfs-$kflavor
}
EOF
    log "bootloader installed ($([ "$uefi" = 1 ] && echo "grub-efi $ESP_MOUNT" || echo "grub-bios MBR /dev/$MAMMOTH_BOOT_DRIVE"))"
}

win_apply() { # windows apply-image phase one (boot.installer=agent, 方案 A)
    # Lay the image down, inject the first-boot config in-wim, then reboot.
    # The ESP stays EMPTY by design: the boot store is generated natively by
    # bcdboot in the boot-two WinPE (the orchestration re-arms a wimboot
    # tree on the applied report) — a hand-patched store never passes NT's
    # BcdOpenStore (0xC0000098, real-machine 9/22).
    # wlog: progress to the console AND the engine diag channel — the VGA
    # console freezes once /dev/console lands on ttyS0 (after the initramfs),
    # so the diag POSTs are the live observability during the silent phases.
    wlog() {
        log "$*"
        [ -n "$MAMMOTH_WIN_DIAG" ] && wget -q -T 5 -O /dev/null --post-data="$(cut -d" " -f1 /proc/uptime) $*" "$MAMMOTH_WIN_DIAG/win-progress" 2>/dev/null
        return 0
    }
    wlog "win_apply start (runtime $SCRIPT_VERSION)"
    # toolchain: the pinned wimlib/mkntfs closure rides the overlay
    # (assets/win-apply — same alpine release as this carrier); pool tools
    # cover partitioning.
    export LD_LIBRARY_PATH=/usr/local/mammoth-win/usr/lib
    WIMLIB=/usr/local/mammoth-win/usr/bin/wimlib-imagex
    MKNTFS=/usr/local/mammoth-win/usr/sbin/mkntfs
    [ -x "$WIMLIB" ] && [ -x "$MKNTFS" ] || { log "windows toolchain missing from overlay"; return 1; }
    apk add --no-cache sfdisk partx util-linux dosfstools >/dev/console 2>&1
    wlog "pool tools installed"
    uefi=0; [ -d /sys/firmware/efi ] && uefi=1
    wlog "firmware check done (uefi=$uefi)"
    [ "$uefi" = 1 ] || { log "windows apply is UEFI-only"; return 1; }

    disk=$(head -1 "$DISKS" | cut -d'|' -f1)
    [ -n "$disk" ] || { log "no disk in plan"; return 1; }
    resolve_disks || return 1
    disk=$(head -1 "$DISKS")
    # the OS partition number: the "/" line's position in the plan order
    osnum=0; n=0
    while IFS='|' read -r d mount fs size flags; do
        [ "$d" = "$disk" ] || continue
        n=$((n+1)); [ "$mount" = "/" ] && osnum=$n
    done < "$PARTS"
    [ "$osnum" -ge 1 ] || { log "plan has no OS partition"; return 1; }
    wlog "disk resolved: $disk os-part $osnum"

    # GPT table: ESP/MSR/NTFS in the plan's final order (the same shape the
    # setup path renders into DiskConfiguration)
    tbl=/tmp/mammoth-win-table
    echo "label: gpt" > "$tbl"
    while IFS='|' read -r d mount fs size flags; do
        [ "$d" = "$disk" ] || continue
        case "$fs" in
            vfat) type="C12A7328-F81F-11D2-BA4B-00A0C93EC93B" ;;
            ntfs) type="EBD0A0A2-B9E5-4433-87C0-68B6B72699C7" ;;
            *)    type="E3C9E316-0B5C-4DB8-817D-F92DF00215AE" ;; # MSR
        esac
        if [ "$size" = "-" ] || [ -z "$size" ]; then
            echo "type=$type" >> "$tbl"
        else
            echo "size=$((size * 2048)), type=$type" >> "$tbl"
        fi
    done < "$PARTS"
    wipefs -a "/dev/$disk" >>/tmp/mammoth-storage.err 2>&1
    attempt=1
    while : ; do
        if sfdisk --force "/dev/$disk" < "$tbl" >>/tmp/mammoth-storage.err 2>&1; then break; fi
        if [ $attempt -ge 3 ]; then log "storage: sfdisk $disk FAILED after $attempt attempts"; return 1; fi
        log "storage: sfdisk attempt $attempt failed, retrying"
        attempt=$((attempt+1)); sleep 3; mdev -s 2>/dev/null
    done
    partx -a "/dev/$disk" >/dev/console 2>&1; mdev -s 2>/dev/null
    wlog "GPT table written"

    ESP_NODE=""; OS_NODE=""
    n=1
    while IFS='|' read -r d mount fs size flags; do
        [ "$d" = "$disk" ] || continue
        node="/dev/$(partnode "$disk" "$n")"
        case "$fs" in
            vfat) mkfs.vfat -F 32 "$node" >>/tmp/mammoth-storage.err 2>&1 && ESP_NODE="$node" ;;
            ntfs) "$MKNTFS" -Q -F "$node" -L WINDOWS >>/tmp/mammoth-storage.err 2>&1 && OS_NODE="$node" ;;
        esac
        n=$((n+1))
    done < "$PARTS"
    [ -n "$OS_NODE" ] || { log "storage: NTFS volume format FAILED"; return 1; }
    wlog "volumes formatted (esp=$ESP_NODE ntfs=$OS_NODE)"

    # stage the wim in the memory root — the precheck fails EARLY (before
    # partitioning ate the old layout? no: partitioning already ran) when the
    # machine plainly cannot hold the image, so the bail detail is honest.
    memkb=$(awk '/MemTotal/ {print $2}' /proc/meminfo)
    wimkb=$(wget -q --spider --server-response "$MAMMOTH_WIN_WIM" 2>&1 | awk '/[Cc]ontent-[Ll]ength/ {len=$NF} END{print int(len/1024)}')
    needkb=$((wimkb + 2097152))
    [ "$memkb" -ge "$needkb" ] || { log "RAM precheck failed: ${memkb}kB < wim ${wimkb}kB + 2G staging headroom"; return 1; }
    log "fetching install.wim ($((wimkb / 1024)) MiB)"
    wlog "fetching install.wim ($((wimkb / 1024)) MiB)"
    wget -q -T 30 -t 3 -O /tmp/win.wim "$MAMMOTH_WIN_WIM" || { log "wim fetch failed"; wlog "wim fetch FAILED"; return 1; }
    wlog "wim fetched"

    # first-boot configuration is injected INTO the wim before apply
    # (wimlib update — the same mechanism as the builder's SetupComplete
    # pair): mounting the freshly-written NTFS volume through ntfs3 hung on
    # real hardware (2288H 9/22 — apply finished, the mount never returned),
    # and the in-wim injection removes the ntfs3 dependency entirely.
    wget -q -T 20 -O /tmp/win-unattend.xml "$MAMMOTH_WIN_UNATTEND" || return 1
    wget -q -T 20 -O /tmp/win-task.json "$MAMMOTH_WIN_TASKJSON" || return 1
    wget -q -T 20 -O /tmp/win-specialize.xml "$MAMMOTH_WIN_SPECIALIZE" || return 1
    wlog "injecting unattend + task.json + specialize into the wim"
    # delete-then-add: wimlib add refuses an existing destination (the same
    # idempotency dance the builder's SetupComplete injection uses)
    "$WIMLIB" update /tmp/win.wim "$MAMMOTH_WIN_INDEX" --command="delete /Windows/Panther/unattend.xml" >/dev/console 2>&1
    "$WIMLIB" update /tmp/win.wim "$MAMMOTH_WIN_INDEX" --command="delete /Windows/Setup/Scripts/task.json" >/dev/console 2>&1
    "$WIMLIB" update /tmp/win.wim "$MAMMOTH_WIN_INDEX" --command="add /tmp/win-unattend.xml /Windows/Panther/unattend.xml" >>/tmp/mammoth-storage.err 2>&1 \
        || { log "unattend injection FAILED"; wlog "unattend injection FAILED"; rm -f /tmp/win.wim; return 1; }
    "$WIMLIB" update /tmp/win.wim "$MAMMOTH_WIN_INDEX" --command="add /tmp/win-task.json /Windows/Setup/Scripts/task.json" >>/tmp/mammoth-storage.err 2>&1 \
        || { log "task.json injection FAILED"; wlog "task.json injection FAILED"; rm -f /tmp/win.wim; return 1; }
    "$WIMLIB" update /tmp/win.wim "$MAMMOTH_WIN_INDEX" --command="delete /Windows/System32/Sysprep/ActionFiles/Specialize.xml" >/dev/console 2>&1
    "$WIMLIB" update /tmp/win.wim "$MAMMOTH_WIN_INDEX" --command="add /tmp/win-specialize.xml /Windows/System32/Sysprep/ActionFiles/Specialize.xml" >>/tmp/mammoth-storage.err 2>&1 \
        || { log "specialize injection FAILED"; wlog "specialize injection FAILED"; rm -f /tmp/win.wim; return 1; }
    wlog "wim updated (unattend + task.json + specialize stripped)"

    log "applying image $MAMMOTH_WIN_INDEX to $OS_NODE"
    wlog "wimlib apply started"
    "$WIMLIB" apply /tmp/win.wim "$MAMMOTH_WIN_INDEX" "$OS_NODE" --no-acls >>/tmp/mammoth-storage.err 2>&1 \
        || { log "wimlib apply FAILED"; wlog "wimlib apply FAILED"; rm -f /tmp/win.wim; return 1; }
    rm -f /tmp/win.wim
    wlog "wimlib apply finished"
    # Done — no ESP boot files, no NVRAM entry here. The boot-two wimboot
    # tree (armed by the orchestration on the applied report) boots a WinPE
    # whose startnet runs bcdboot against the volumes laid out above: the
    # store is then NT-native, and specialize's BCD module opens it like it
    # opened setup.exe's own.
    log "windows image applied: wim $MAMMOTH_WIN_INDEX -> $OS_NODE (first boot pending bcdboot stage)"
    return 0
}

# keep the runtime's stderr on the console: openrc swallows it, and the
# trace (set -x) plus every tool error message is the SOL diagnostic surface
exec 2>>/dev/console
log "agent start (kernel $(uname -r))"
find_plan || bail plan "agent-plan.sh not found (media scan + ${BASE:-<no base>}/agent-plan.sh)"
. "$PLAN"
[ -n "$MAMMOTH_COMPLETE_URL" ] || bail plan "plan carries no COMPLETE_URL"
if [ "$MAMMOTH_WIN_MODE" = "apply" ]; then
    log "windows apply-image flow"
    win_apply || bail win_apply "windows apply failed: $(tail -c 220 /tmp/mammoth-storage.err 2>/dev/null | tr '\n' ' ' | tr -d '"\\')"
    sync
    if report applied "windows image applied (agent apply-image) — first boot pending"; then
        log "rebooting into first boot"
        reboot -f
    fi
    bail report "completion report could not be delivered"
fi
log "plan loaded: $(cat "$DISKS" | tr '\n' ' ')"
bootstrap_tools || bail tools "agent tooling install failed"
run_stage pre_install || bail pre_install "script failed"
if ! apply_storage 2>>/tmp/mammoth-storage.err; then
    bail storage "partition/format/mount failed: $(tail -c 220 /tmp/mammoth-storage.err 2>/dev/null | tr '\n' ' ' | tr -d '"\\')"
fi
install_base || bail packages "base install from pool failed"
configure_target || bail config "system configuration failed"
install_bootloader || bail bootloader "bootloader install failed"
run_stage post_install || bail post_install "script failed"
sync
if report ok "installed by mammoth agent"; then
    log "install complete — rebooting into the new system"
    reboot -f
fi
bail report "completion report could not be delivered"
`
}
