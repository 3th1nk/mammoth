package nocloud

// NoCloud seed rendering (roadmap「镜像直写通路」slice 2, shared by the
// image-consuming pathways — dd direct-write and bootc OCI install):
// identity/network/access intent lands as a cloud-init seed on a CIDATA
// partition the runtime appends after the image lands — the image boots its
// own bootloader, its cloud-init picks the seed up on first boot, and the
// spec's intent survives the transport. This is the engine's incremental
// value over a bare image drop: the address/credential contract travels
// with the install. IMAGE REQUIREMENT: the target image must carry
// cloud-init (the NoCloud datasource is cloud-init's, not the engine's).

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"

	"github.com/3th1nk/mammoth/internal/render"
	"github.com/3th1nk/mammoth/internal/render/autoinstall"
)

// seedIntent reports whether any intent block needs the seed.
func SeedIntent(in render.InstallInputs) bool {
	return in.Hostname != "" || in.RootPassword != "" ||
		len(in.SSHPublicKeys) > 0 || len(in.Network) > 0
}

// seedFiles renders the NoCloud payload: meta-data, user-data and (when the
// spec declares interfaces) network-config — the exact file set cloud-init's
// NoCloud datasource reads from the filesystem labeled CIDATA.
func SeedFiles(in render.InstallInputs) (map[string]string, error) {
	if !SeedIntent(in) {
		return nil, nil
	}
	meta := "# instance-id ties the seed to this task (rerun = new instance-id = re-apply)\ninstance_id: mammoth-" + in.TaskToken + "\n"
	if in.Hostname != "" {
		meta += "local_hostname: " + in.Hostname + "\n"
	}

	var u strings.Builder
	u.WriteString("#cloud-config\n")
	u.WriteString("disable_root: false\n")
	if in.RootPassword != "" {
		// crypt hash, never plaintext (the 2288H plaintext-in-shadow lesson):
		// cloud-init's chpasswd applies $6$ hashes verbatim.
		crypt := autoinstall.CryptSHA512(in.RootPassword, randomSalt())
		u.WriteString("ssh_pwauth: true\n")
		u.WriteString("chpasswd:\n  expire: false\n  list: |\n    root:" + crypt + "\n")
	}
	if len(in.SSHPublicKeys) > 0 {
		u.WriteString("users:\n  - name: root\n    ssh_authorized_keys:\n")
		for _, k := range in.SSHPublicKeys {
			u.WriteString("      - " + k + "\n")
		}
	}
	user := u.String()

	var net string
	if len(in.Network) > 0 {
		n, err := NetworkConfig(in.Network)
		if err != nil {
			return nil, err
		}
		net = n
	}

	files := map[string]string{"meta-data": meta, "user-data": user}
	if net != "" {
		files["network-config"] = net
	}
	return files, nil
}

// networkConfig renders cloud-init network-config v2 (netplan shape) for the
// supported subset: plain interfaces matched by MAC with static addressing —
// bonds/vlans stay an honest reject (the autoinstall dialect owns their
// netplan rendering; carrying it here is a later slice).
func NetworkConfig(entries []render.NetworkEntry) (string, error) {
	var b strings.Builder
	b.WriteString("version: 2\nethernets:\n")
	n := 0
	for _, e := range entries {
		if e.Bond != nil || e.VLAN != nil {
			return "", fmt.Errorf("bond/vlan intent is not carried by the image pathways yet (plain interfaces only)")
		}
		if e.Match == nil || e.Match.MAC == "" {
			return "", fmt.Errorf("image-pathway network entries must match by MAC (the installed system's naming is the image's own)")
		}
		n++
		name := fmt.Sprintf("nic%d", n)
		b.WriteString("  " + name + ":\n")
		b.WriteString("    match:\n      macaddress: \"" + strings.ToLower(e.Match.MAC) + "\"\n")
		b.WriteString("    set-name: " + name + "\n")
		if len(e.Addresses) == 0 {
			b.WriteString("    dhcp4: true\n")
			continue
		}
		b.WriteString("    dhcp4: false\n")
		for _, a := range e.Addresses {
			b.WriteString("    addresses:\n      - " + a + "\n")
		}
		for _, r := range e.Routes {
			to := r.To
			if to == "" {
				to = "default"
			}
			b.WriteString("    routes:\n      - to: " + to + "\n        via: " + r.Via + "\n")
		}
		if len(e.Nameservers) > 0 {
			b.WriteString("    nameservers:\n      addresses:\n")
			for _, ns := range e.Nameservers {
				b.WriteString("        - " + ns + "\n")
			}
		}
		if e.MTU > 0 {
			fmt.Fprintf(&b, "    mtu: %d\n", e.MTU)
		}
	}
	if n == 0 {
		return "", nil
	}
	return b.String(), nil
}

// seedPlanVars emits the base64-encoded seed files into the plan — the
// runtime decodes them onto the CIDATA partition (busybox base64 -d).
func SeedPlanVars(in render.InstallInputs) (string, error) {
	files, err := SeedFiles(in)
	if err != nil {
		return "", err
	}
	if files == nil {
		return "", nil
	}
	var b strings.Builder
	b.WriteString("MAMMOTH_SEED=1\n")
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		enc := base64.StdEncoding.EncodeToString([]byte(files[name]))
		fmt.Fprintf(&b, "MAMMOTH_SEED_%s=%s\n", seedVarName(name), enc)
	}
	return b.String(), nil
}

// seedVarName maps the file name to a plan variable suffix (user-data →
// USER_DATA); names are engine-owned, so the mapping is total.
func seedVarName(file string) string {
	return strings.ToUpper(strings.ReplaceAll(strings.ReplaceAll(file, "-", "_"), ".", "_"))
}

// randomSalt is the crypt hash salt source (8 random bytes, hex form).
func randomSalt() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "mammoth0" // deterministic fallback — worst case a weaker salt
	}
	return hex.EncodeToString(b)
}
