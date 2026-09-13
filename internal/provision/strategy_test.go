package provision

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/3th1nk/mammoth/internal/bmc"
	"github.com/3th1nk/mammoth/internal/store"
)

func TestBootStrategyFor(t *testing.T) {
	specWith := func(strategy string) *installSpecView {
		var s installSpecView
		s.Boot.Strategy = strategy
		return &s
	}

	t.Run("default is virtual media", func(t *testing.T) {
		s, err := (&Executor{}).bootStrategyFor(&installSpecView{})
		if err != nil || s.name() != strategyVirtualMedia {
			t.Fatalf("got %v %v", s, err)
		}
	})

	t.Run("explicit virtual media", func(t *testing.T) {
		s, err := (&Executor{}).bootStrategyFor(specWith("virtual_media"))
		if err != nil || s.name() != strategyVirtualMedia {
			t.Fatalf("got %v %v", s, err)
		}
	})

	t.Run("pxe without the netboot repo fails fast", func(t *testing.T) {
		if _, err := (&Executor{}).bootStrategyFor(specWith("pxe")); err == nil {
			t.Fatal("want NETBOOT_UNAVAILABLE")
		} else if !strings.Contains(err.Error(), "MAMMOTH_PXE_ENABLED") {
			t.Fatalf("unhelpful error: %v", err)
		}
	})

	t.Run("pxe with the repo armed", func(t *testing.T) {
		e := &Executor{Netboot: &store.NetbootRepo{}}
		s, err := e.bootStrategyFor(specWith("pxe"))
		if err != nil || s.name() != strategyPXE {
			t.Fatalf("got %v %v", s, err)
		}
	})

	t.Run("unknown strategy rejected", func(t *testing.T) {
		if _, err := (&Executor{}).bootStrategyFor(specWith("carrier_pigeon")); err == nil {
			t.Fatal("want SCHEMA_INVALID_BOOT_STRATEGY")
		}
	})

	t.Run("deployment default pxe without service is a config conflict", func(t *testing.T) {
		e := &Executor{BootStrategyDefault: "pxe"}
		if _, err := e.bootStrategyFor(&installSpecView{}); err == nil {
			t.Fatal("want NETBOOT_UNAVAILABLE for a pxe-default runner without the service")
		}
	})
}

func TestMacsFor(t *testing.T) {
	hw := bmc.HardwareView{NICs: []bmc.NICView{
		{Name: "eth0", MAC: "AA:BB:CC:DD:EE:01"},
		{Name: "eth1", MAC: "aa-bb-cc-dd-ee-02"},
		{Name: "eth2", MAC: ""},                  // redfish drops MAC-less ports, but stay safe
		{Name: "eth3", MAC: "AA:BB:CC:DD:EE:01"}, // duplicate across ports
	}}
	got := macsFor(hw, nil)
	want := []string{"aa:bb:cc:dd:ee:01", "aa:bb:cc:dd:ee:02"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}

	// A network match.mac ADDS a candidate (the boot NIC may be unpowered in
	// the inventory but be the one the firmware uses).
	withMatch := []networkView{{
		Match: &struct {
			MAC        string `json:"mac"`
			PCIAddress string `json:"pci_address"`
			Name       string `json:"name"`
		}{MAC: "aa:bb:cc:dd:ee:ff"},
	}}
	got = macsFor(hw, withMatch)
	if len(got) != 3 || got[0] != "aa:bb:cc:dd:ee:ff" {
		t.Fatalf("match mac not added first: %v", got)
	}
}

func TestReleaseBootPayloadDispatch(t *testing.T) {
	mediaDir := t.TempDir()

	// Legacy task context (no boot_strategy marker): virtual-media cleanup —
	// the boot ISO disappears from the media repo.
	ictx := &installTaskContext{Token: "tok1"}
	iso := filepath.Join(mediaDir, "boot-tok1.iso")
	if err := os.WriteFile(iso, []byte("iso"), 0o644); err != nil {
		t.Fatal(err)
	}
	e := &Executor{MediaDir: mediaDir}
	raw, _ := json.Marshal(ictx)
	e.releaseBootPayload(t.Context(), &store.Task{ID: "tst_1", Context: raw}, ictx, "terminal failure")
	if _, err := os.Stat(iso); !os.IsNotExist(err) {
		t.Fatal("legacy context must clean the boot ISO")
	}

	// PXE context: tree removal (registry rows need a store — exercised in
	// the PG suite via DeleteByTask).
	treeDir := t.TempDir()
	token := "tok2"
	if err := os.MkdirAll(filepath.Join(treeDir, token), 0o755); err != nil {
		t.Fatal(err)
	}
	pxeIctx := &installTaskContext{
		Token: token, BootStrategy: "pxe",
		Netboot: &netbootRecord{Token: token, MACs: []string{"aa:bb:cc:dd:ee:01"}},
	}
	e = &Executor{MediaDir: mediaDir, BootTreeDir: treeDir}
	raw, _ = json.Marshal(pxeIctx)
	e.releaseBootPayload(t.Context(), &store.Task{ID: "tst_2", Context: raw}, pxeIctx, "completed")
	if _, err := os.Stat(filepath.Join(treeDir, token)); !os.IsNotExist(err) {
		t.Fatal("pxe release must remove the boot tree")
	}

	// Traversal guard: a malicious token never escapes the tree root.
	evil := &installTaskContext{Token: "../escape", BootStrategy: "pxe",
		Netboot: &netbootRecord{Token: "../escape"}}
	e.releaseBootPayload(t.Context(), &store.Task{ID: "tst_3"}, evil, "completed")
	if _, err := os.Stat(filepath.Join(treeDir, "escape")); !os.IsNotExist(err) {
		t.Fatal("traversal token must not remove outside the tree root")
	}
}
