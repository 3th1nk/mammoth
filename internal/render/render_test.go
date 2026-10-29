package render

import (
	"slices"
	"strings"
	"testing"
)

func TestSyslogHost(t *testing.T) {
	cases := []struct{ in, want string }{
		{"https://m/render/tok", "m"},
		{"http://10.0.0.1:8080/render/tok", "10.0.0.1"}, // port stripped — the arg carries none
		{"", ""},
		{"::bad url::", ""},
	}
	for _, c := range cases {
		if got := SyslogHost(c.in); got != c.want {
			t.Errorf("SyslogHost(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// Bond declaration normalization (docs/04-install-spec.md §5.2): numeric
// aliases fold to kernel names, unknown modes reject with the accepted list,
// and the miimon default applies only when the spec declares no monitoring
// at all (arp_interval is its own, mutually exclusive mechanism).
func TestNetBondNormalized(t *testing.T) {
	b, err := (&NetBond{SlavesMACs: []string{"aa:bb:cc:dd:ee:01"}, Mode: "4"}).Normalized()
	if err != nil {
		t.Fatalf("mode 4: %v", err)
	}
	if b.Mode != "802.3ad" {
		t.Errorf("numeric alias must fold to the kernel name, got %q", b.Mode)
	}
	if b.Params["miimon"] != "100" {
		t.Errorf("miimon default missing, got %v", b.Params)
	}

	b, err = (&NetBond{SlavesMACs: []string{"aa:bb:cc:dd:ee:01"}, Mode: "Active-Backup",
		Params: map[string]string{"miimon": "250"}}).Normalized()
	if err != nil {
		t.Fatalf("active-backup: %v", err)
	}
	if b.Mode != "active-backup" || b.Params["miimon"] != "250" {
		t.Errorf("declared miimon must win over the default, got %q %v", b.Mode, b.Params)
	}

	b, err = (&NetBond{SlavesMACs: []string{"aa:bb:cc:dd:ee:01"}, Mode: "active-backup",
		Params: map[string]string{"arp_interval": "500"}}).Normalized()
	if err != nil {
		t.Fatalf("arp monitoring: %v", err)
	}
	if _, declared := b.Params["miimon"]; declared {
		t.Errorf("arp_interval excludes miimon, got %v", b.Params)
	}

	for mode, name := range map[string]string{"lacp": "unknown name", "7": "number out of range", "": "empty"} {
		if _, err := (&NetBond{SlavesMACs: []string{"aa:bb:cc:dd:ee:01"}, Mode: mode}).Normalized(); err == nil || !strings.Contains(err.Error(), "active-backup") {
			t.Errorf("%s mode %q must reject with the accepted list, got %v", name, mode, err)
		}
	}
	if _, err := (&NetBond{Mode: "1"}).Normalized(); err == nil {
		t.Errorf("bond without slaves must reject")
	}
	if _, err := (&NetBond{SlavesMACs: []string{"aa:bb:cc:dd:ee:01", ""}, Mode: "1"}).Normalized(); err == nil {
		t.Errorf("slave without a mac must reject (iface_by_mac would emit garbage)")
	}

	// Normalized copies: the receiver's params must stay untouched.
	orig := &NetBond{SlavesMACs: []string{"aa:bb:cc:dd:ee:01"}, Mode: "1"}
	if _, err := orig.Normalized(); err != nil {
		t.Fatal(err)
	}
	if _, declared := orig.Params["miimon"]; orig.Params != nil && declared {
		t.Errorf("Normalized must not mutate the receiver, got %v", orig.Params)
	}
}

// Param vocabulary (docs/04-install-spec.md §5.2): the spec accepts the
// cloud-init v2 / netplan spellings and folds them onto canonical kernel
// names; the same key under two spellings rejects, unknown keys reject
// (a typo'd param is silently inert downstream — the exact failure class
// this catches), and multi-value targets take the space-joined canonical
// form.
func TestNetBondParamVocabulary(t *testing.T) {
	b, err := (&NetBond{SlavesMACs: []string{"aa:bb:cc:dd:ee:01"}, Mode: "1", Params: map[string]string{
		"mii-monitor-interval": "250",
		"lacp-rate":            "fast",
	}}).Normalized()
	if err != nil {
		t.Fatalf("v2 spellings: %v", err)
	}
	if b.Params["miimon"] != "250" || b.Params["lacp_rate"] != "fast" {
		t.Errorf("v2 spellings must fold to kernel names, got %v", b.Params)
	}

	if _, err := (&NetBond{SlavesMACs: []string{"aa:bb:cc:dd:ee:01"}, Mode: "1", Params: map[string]string{
		"miimon": "100", "mii-monitor-interval": "100",
	}}).Normalized(); err == nil || !strings.Contains(err.Error(), "declared twice") {
		t.Errorf("dual spelling of one param must reject, got %v", err)
	}
	if _, err := (&NetBond{SlavesMACs: []string{"aa:bb:cc:dd:ee:01"}, Mode: "1",
		Params: map[string]string{"miimonn": "100"}}).Normalized(); err == nil || !strings.Contains(err.Error(), "unknown") {
		t.Errorf("typo'd param must reject with the accepted list, got %v", err)
	}
	// kernel-only names stay valid spec vocabulary (the ubuntu dialect
	// rejects them at ITS emission, not here)
	if _, err := (&NetBond{SlavesMACs: []string{"aa:bb:cc:dd:ee:01"}, Mode: "1",
		Params: map[string]string{"use_carrier": "0"}}).Normalized(); err != nil {
		t.Errorf("kernel-only param must be accepted at the spec layer, got %v", err)
	}
	// comma lists normalize to the space-joined canonical form
	b, err = (&NetBond{SlavesMACs: []string{"aa:bb:cc:dd:ee:01"}, Mode: "1", Params: map[string]string{
		"arp-ip-targets": "10.0.0.1,10.0.0.2",
	}}).Normalized()
	if err != nil {
		t.Fatal(err)
	}
	if b.Params["arp_ip_target"] != "10.0.0.1 10.0.0.2" {
		t.Errorf("arp target list must normalize to spaces, got %q", b.Params["arp_ip_target"])
	}
}

// BondExpectations is the post-install verification contract: declaration
// order naming, normalized mode/miimon, sorted lowercase MACs.
func TestBondExpectations(t *testing.T) {
	checks, err := BondExpectations([]NetworkEntry{
		{Match: &NetMatch{MAC: "aa:bb:cc:dd:ee:09"}, Addresses: []string{"10.0.0.9/24"}},
		{Bond: &NetBond{SlavesMACs: []string{"AA:BB:CC:DD:EE:02", "AA:BB:CC:DD:EE:01"}, Mode: "4",
			Params: map[string]string{"lacp_rate": "fast"}}, Addresses: []string{"172.16.1.11/24"}},
		{Bond: &NetBond{SlavesMACs: []string{"AA:BB:CC:DD:EE:03", "AA:BB:CC:DD:EE:04"}, Mode: "active-backup",
			Params: map[string]string{"arp_interval": "500"}}, Addresses: []string{"172.16.2.11/24"}},
	})
	if err != nil {
		t.Fatalf("BondExpectations: %v", err)
	}
	if len(checks) != 2 {
		t.Fatalf("two bonds declared, got %d: %+v", len(checks), checks)
	}
	if checks[0].Name != "bond0" || checks[0].Mode != "802.3ad" || checks[0].MIImon != 100 {
		t.Errorf("first bond wrong: %+v", checks[0])
	}
	if !slices.Equal(checks[0].SlaveMACs, []string{"aa:bb:cc:dd:ee:01", "aa:bb:cc:dd:ee:02"}) {
		t.Errorf("slave macs must be sorted lowercase: %v", checks[0].SlaveMACs)
	}
	// arp monitoring: no miimon expectation, active member required
	if checks[1].MIImon != 0 || !checks[1].Active {
		t.Errorf("second bond wrong: %+v", checks[1])
	}
}

// TestSyslogTarget pins the sink-port rendering: the default (0/514) keeps
// the bare-host argument byte-for-byte, a moved sink renders host:port —
// busybox syslogd -R (casper), anaconda inst.syslog and d-i all accept it.
func TestSyslogTarget(t *testing.T) {
	cases := []struct {
		port int
		want string
	}{
		{0, "192.168.1.107"},
		{514, "192.168.1.107"},
		{5514, "192.168.1.107:5514"},
	}
	for _, c := range cases {
		if got := SyslogTarget("192.168.1.107", c.port); got != c.want {
			t.Errorf("SyslogTarget(host, %d) = %q, want %q", c.port, got, c.want)
		}
	}
}
