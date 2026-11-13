package netboot

import (
	"context"
	"net"
	"testing"
)

// The pool-mode lease gate for the zero-registration entry
// (docs/13-headless-design.md §3.1): on address-authority deployments
// (MAMMOTH_PXE_DHCP_POOL, no site DHCP) strict consumer UEFI ROMs refuse a
// proxy offer — lease and boot information must arrive in one packet (the
// office-PC finding, 2026-11-12). The gate admits sighted unknown MACs to
// the lease; MACs the PXE wire never sighted stay refused (the pool is not
// the room's address service — 2288H).
//
// The fake eligibility wiring mirrors serve's: the observation hook records
// the sighting BEFORE the gate consults eligibility, so the very first
// DISCOVER of a machine already qualifies.
func newEnrollPoolServer(t *testing.T) *Server {
	t.Helper()
	pool, err := ParseDHCPPool("10.0.0.10-12", nil)
	if err != nil {
		t.Fatalf("pool parse: %v", err)
	}
	sighted := map[string]bool{}
	registered := map[string]bool{"52:54:00:aa:aa:aa": true} // a registered machine
	return testServer(t, Options{
		NextServer:   []byte{10, 0, 0, 1},
		BaseURL:      "http://10.0.0.1:8080",
		UEFIPlainNBP: true, // the office deployment's UEFI shape
		DHCP:         pool,
		Resolver: ResolverFunc(func(_ context.Context, _ string) (*Entry, error) {
			return nil, nil // no armed entries: everything unknown
		}),
		OnObserve: func(mac string, _ Arch) {
			sighted[mac] = true // TouchByMAC's stand-in: the sighting lands first
		},
		EnrollEligible: func(mac string) bool {
			return sighted[mac] && !registered[mac]
		},
	})
}

var enrollMAC = [6]byte{0x52, 0x54, 0x00, 0xd2, 0x21, 0x23}

// The first DISCOVER of a brand-new machine must get lease AND boot
// information in one OFFER — the strict-ROM contract.
func TestPoolEnrollFirstDiscoverGetsLeaseAndBoot(t *testing.T) {
	s := newEnrollPoolServer(t)
	req := discover(0x11111111, enrollMAC,
		option{optVendorClass, []byte("PXEClient:Arch:00007:UNDI:003016")},
		option{optArch, u16opt(7)},
	)
	reply, _ := s.handle(req, 67, testSrc)
	if reply == nil {
		t.Fatal("first enroll DISCOVER must be answered (lease+boot in one offer)")
	}
	p, err := parse(reply)
	if err != nil {
		t.Fatalf("reply does not parse: %v", err)
	}
	if p.messageType() != msgOffer {
		t.Fatalf("want OFFER, got %d", p.messageType())
	}
	if net.IP(p.yiaddr[:]).String() == "0.0.0.0" {
		t.Error("enroll offer must carry a lease (yiaddr), strict ROMs refuse the split")
	}
	bf, ok := p.options[optBootfile]
	if !ok || string(bf) != "ipxe-amd64.efi" {
		t.Errorf("bootfile = %q, want ipxe-amd64.efi (UEFIPlainNBP)", bf)
	}
}

// After the ROM phase, the plain renewals on the same MAC (iPXE's re-DHCP,
// the enroll kernel's ip=dhcp) get a LEASE ONLY — never boot parameters, or
// every plain host would chainload iPXE.
func TestPoolEnrollPlainRenewalLeaseOnly(t *testing.T) {
	s := newEnrollPoolServer(t)
	// Sight the MAC through its PXE phase first (same server, same ledger).
	pxe := discover(0x22222222, enrollMAC,
		option{optVendorClass, []byte("PXEClient:Arch:00007:UNDI:003016")},
		option{optArch, u16opt(7)},
	)
	if r, _ := s.handle(pxe, 67, testSrc); r == nil {
		t.Fatal("pxe phase must be answered")
	}
	// Now the plain kernel DISCOVER (no option 60 — the ip=dhcp shape).
	req := discover(0x33333333, enrollMAC)
	reply, _ := s.handle(req, 67, testSrc)
	if reply == nil {
		t.Fatal("sighted MAC's plain renewal must be leased (the enroll kernel needs ip=dhcp)")
	}
	p, _ := parse(reply)
	if net.IP(p.yiaddr[:]).String() == "0.0.0.0" {
		t.Error("plain renewal must carry a lease")
	}
	if _, ok := p.options[optBootfile]; ok {
		t.Error("plain renewal must never carry boot parameters")
	}
}

// The strictness the gate must keep in both configurations:
//   - Enroll OFF (EnrollEligible nil): the armed-entries-only contract —
//     unknown PXE clients are sighted (the ledger still records the room)
//     but never leased (2288H: VMs drained the pool through this hole).
//   - Enroll ON: a plain client whose MAC never PXE-sighted (printer,
//     laptop) is still refused — the ledger keys on PXE sightings.
func TestPoolEnrollForeignClientsStayRefused(t *testing.T) {
	pool, err := ParseDHCPPool("10.0.0.10-12", nil)
	if err != nil {
		t.Fatalf("pool parse: %v", err)
	}
	foreign := [6]byte{0x52, 0x54, 0x00, 0xee, 0xee, 0xee}

	// Enroll off: sighted-but-unknown PXE clients stay leaseless.
	strict := testServer(t, Options{
		NextServer: []byte{10, 0, 0, 1},
		BaseURL:    "http://10.0.0.1:8080",
		DHCP:       pool,
		Resolver: ResolverFunc(func(_ context.Context, _ string) (*Entry, error) {
			return nil, nil
		}),
	})
	req := discover(0x44444444, foreign,
		option{optVendorClass, []byte("PXEClient:Arch:00007:UNDI:003016")},
		option{optArch, u16opt(7)},
	)
	if r, _ := strict.handle(req, 67, testSrc); r != nil {
		t.Error("enroll off: unknown PXE client must not be leased (pool serves installs only)")
	}
	if r, _ := strict.handle(discover(0x55555555, foreign), 67, testSrc); r != nil {
		t.Error("enroll off: plain foreign client must not be leased")
	}

	// Enroll on: a plain client that never PXE-sighted stays refused.
	enroll := newEnrollPoolServer(t)
	fresh := [6]byte{0x52, 0x54, 0x00, 0x99, 0x99, 0x99}
	if r, _ := enroll.handle(discover(0x56565656, fresh), 67, testSrc); r != nil {
		t.Error("enroll on: never-sighted plain client must not be leased")
	}
}

// A registered machine's MAC never enters the enroll path — its facts live
// in the hardware view and its leases come from armed entries (the
// ingestion auto-match rule, commit c64df07's shape).
func TestPoolEnrollRegisteredMACNotEligible(t *testing.T) {
	s := newEnrollPoolServer(t)
	regMAC := [6]byte{0x52, 0x54, 0x00, 0xaa, 0xaa, 0xaa}
	req := discover(0x66666666, regMAC,
		option{optVendorClass, []byte("PXEClient:Arch:00007:UNDI:003016")},
		option{optArch, u16opt(7)},
	)
	// The observation hook fires (it is the sighting feed), but eligibility
	// refuses: registered MACs lease via armed entries, which this fake
	// resolver has none of — so the reply is silence, never a stray lease.
	if r, _ := s.handle(req, 67, testSrc); r != nil {
		t.Error("registered MAC must not lease through the enroll path")
	}
}
