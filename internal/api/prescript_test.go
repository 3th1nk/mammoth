package api

import (
	"strings"
	"testing"

	"github.com/3th1nk/mammoth/internal/netboot"
)

// The enroll prescript's NIC ledger (2026-11-13 office-PC finding): iPXE's
// ${netX/mac} expansion reports every NIC of the machine; the ledger keeps
// the valid, deduped, primary-excluded remainder. Literal ${...} leftovers
// (an exotic iPXE build without the setting) degrade to primary-only.
func TestLedgerNICs(t *testing.T) {
	got := ledgerNICs("c8:d9:d2:21:23:03",
		"c8:d9:d2:21:23:03,c8:d9:d2:26:a9:4d,,C8-D9-D2-26-AA-BB,${net5/mac}")
	want := []string{"c8:d9:d2:26:a9:4d", "c8:d9:d2:26:aa:bb"}
	if len(got) != len(want) {
		t.Fatalf("ledgerNICs = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("ledgerNICs = %v, want %v", got, want)
		}
	}
	if n := ledgerNICs("c8:d9:d2:21:23:03", ""); len(n) != 0 {
		t.Fatalf("empty macs must yield no extras, got %v", n)
	}
}

func TestRenderEnrollPreScript(t *testing.T) {
	s := netboot.RenderEnrollPreScript("http://172.16.20.250:8091", "c8:d9:d2:21:23:03")
	if !strings.HasPrefix(s, "#!ipxe\n") {
		t.Fatalf("prescript must start with the iPXE magic: %q", s)
	}
	if !strings.Contains(s, "chain http://172.16.20.250:8091/netboot/script?mac=c8:d9:d2:21:23:03&macs=${net0/mac}") {
		t.Fatalf("prescript must chain back with the NIC expansion: %q", s)
	}
	for i := 0; i <= 5; i++ {
		if !strings.Contains(s, "${net"+string(rune('0'+i))+"/mac}") {
			t.Fatalf("prescript must report net%d: %q", i, s)
		}
	}
}
