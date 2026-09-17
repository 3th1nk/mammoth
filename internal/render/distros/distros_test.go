package distros

import (
	"strings"
	"testing"
)

// The declaration file IS the distro support surface: these fixtures pin
// the parameters that encode real-machine lessons (a wrong flag here is a
// broken install — see docs/compat/distros.md incidents).
func TestDeclarations(t *testing.T) {
	all := Declarations()
	if len(all) < 11 {
		t.Fatalf("expected the full known matrix (>=11), got %d: %v", len(all), Names())
	}
	if len(all) != len(Names()) {
		t.Fatalf("Declarations/Names disagree")
	}

	t.Run("kylinv10 carries the V10 workarounds", func(t *testing.T) {
		p := KickstartFor("kylinv10")
		if !p.NetRepair || !p.DeviceByMAC || !p.HostnameViaNetworkCmd || !p.RootExtension {
			t.Fatalf("kylinv10 profile lost the V10 lessons: %+v", p)
		}
	})

	t.Run("centos7 disables the modern paths", func(t *testing.T) {
		p := KickstartFor("centos7")
		if p.HostnameViaNetworkCmd || p.RootExtension || p.NetRepair || p.DeviceByMAC {
			t.Fatalf("centos7 must use %%post hostname and skip root extension: %+v", p)
		}
	})

	t.Run("uniontechos carries the Finish-crash extras", func(t *testing.T) {
		p := KickstartFor("uniontechos")
		if !strings.Contains(p.Extras, "eula --agreed") || !strings.Contains(p.Extras, "user --name=uos") {
			t.Fatalf("uniontechos extras missing EULA/user acks: %q", p.Extras)
		}
	})

	t.Run("rocky10 is uefi-only media", func(t *testing.T) {
		if FirmwareFor("rocky10") != "uefi_only" {
			t.Fatalf("rocky10 firmware = %q", FirmwareFor("rocky10"))
		}
		if FirmwareFor("rocky9") != "all" {
			t.Fatalf("rocky9 firmware = %q", FirmwareFor("rocky9"))
		}
	})

	t.Run("preseed suites ride the declaration", func(t *testing.T) {
		if PreseedFor("debian12").Suite != "bookworm" || PreseedFor("debian13").Suite != "trixie" {
			t.Fatalf("suites wrong: %q %q", PreseedFor("debian12").Suite, PreseedFor("debian13").Suite)
		}
	})

	t.Run("alpine agent profile is complete", func(t *testing.T) {
		p := AgentFor("alpine")
		for _, want := range []string{"alpine-base", "linux-lts", "openssh"} {
			found := false
			for _, pkg := range p.Packages {
				if pkg == want {
					found = true
				}
			}
			if !found {
				t.Fatalf("agent packages missing %q: %v", want, p.Packages)
			}
		}
		if len(p.BootloaderBIOS) == 0 || len(p.BootloaderUEFI) == 0 || len(p.Tools) == 0 {
			t.Fatalf("agent bootloader/tools incomplete: %+v", p)
		}
	})

	t.Run("ubuntu is amd64-only (live-server media)", func(t *testing.T) {
		for _, name := range []string{"ubuntu22", "ubuntu24"} {
			archs := ArchsFor(name)
			if len(archs) != 1 || archs[0] != "amd64" {
				t.Fatalf("%s archs = %v, want [amd64] — live-server ships no arm64 media", name, archs)
			}
		}
	})

	t.Run("every declared pool capability is the system kind", func(t *testing.T) {
		// All current members bundle a system pool. A future boot_pool entry
		// (e.g. an alpine STANDARD-based distro) must be a conscious add —
		// this assertion forces that decision into the open.
		for _, d := range all {
			if d.Pool != PoolSystem {
				t.Fatalf("%s: pool = %q, want system_pool", d.Name, d.Pool)
			}
		}
	})

	t.Run("unknown distros fall back safely", func(t *testing.T) {
		if p := KickstartFor("no-such-distro"); !p.HostnameViaNetworkCmd || !p.RootExtension {
			t.Fatalf("kickstart fallback must stay current-generation defaults: %+v", p)
		}
		if FirmwareFor("no-such-distro") != "all" {
			t.Fatalf("firmware fallback = %q", FirmwareFor("no-such-distro"))
		}
		if PreseedFor("no-such-distro").Suite != "" {
			t.Fatalf("preseed fallback must stay suite-less")
		}
	})
}
