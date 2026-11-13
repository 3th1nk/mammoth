package redfish

// Real-hardware read-only verification suite, gated by:
//
//	MAMMOTH_BMC_E2E='host=192.168.1.72 user=admin pass=...' go test ./internal/bmc/redfish/ -run E2ERO
//
// Exercises every read-only driver method against a live BMC and prints the
// results — the compatibility-check for a new vendor unit (docs/compat/
// README.md's 预检 flow). No writes, no power/boot/media operations.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/3th1nk/mammoth/internal/bmc"
)

func e2eBMC(t *testing.T) (host, user, pass string) {
	t.Helper()
	spec := os.Getenv("MAMMOTH_BMC_E2E")
	if spec == "" {
		t.Skip("MAMMOTH_BMC_E2E not set; skipping real-BMC read-only suite")
	}
	var h, u, p string
	if _, err := fmt.Sscanf(spec, "host=%s user=%s pass=%s", &h, &u, &p); err != nil || h == "" {
		t.Fatalf("bad spec %q — want 'host=IP user=U pass=P'", spec)
	}
	return h, u, p
}

func dump(t *testing.T, label string, v any, err error) {
	t.Helper()
	if err != nil {
		t.Logf("%s: ERROR %v", label, err)
		return
	}
	b, _ := json.Marshal(v)
	t.Logf("%s: %.1200s", label, b)
}

func TestE2ERO(t *testing.T) {
	host, user, pass := e2eBMC(t)
	d := New(true, 60*time.Second)
	ctx := context.Background()
	cred := bmc.Credentials{Username: user, Password: pass}

	info, err := d.Probe(ctx, host, cred)
	dump(t, "Probe", info, err)

	ps, err := d.PowerState(ctx, host, cred)
	dump(t, "PowerState", ps, err)

	hw, err := d.CollectInventory(ctx, host, cred)
	dump(t, "CollectInventory", hw, err)

	hv, err := d.Health(ctx, host, cred)
	dump(t, "Health.health", hv.Health, err)
	t.Logf("Health: %d sensors", len(hv.Sensors))

	sel, err := d.SystemEventLog(ctx, host, cred)
	if err != nil {
		t.Logf("SystemEventLog: ERROR %v", err)
	} else {
		t.Logf("SystemEventLog: %d entries", len(sel))
	}

	fw, err := d.FirmwareInventory(ctx, host, cred)
	if err != nil {
		t.Logf("FirmwareInventory: ERROR %v", err)
	} else {
		t.Logf("FirmwareInventory: %d components", len(fw))
	}

	pd, err := d.PhysicalDrives(ctx, host, cred)
	dump(t, "PhysicalDrives", pd, err)

	bios, err := d.BiosAttributes(ctx, host, cred)
	if err != nil {
		t.Logf("BiosAttributes: ERROR %v", err)
	} else {
		t.Logf("BiosAttributes: %d attributes", len(bios))
	}
}

// TestE2EPhysicalDrives: the Dell-specific regression for the rotating
// Storage collection (docs/compat/dell.md §2), gated by MAMMOTH_DELL_E2E.
func TestE2EPhysicalDrives(t *testing.T) {
	spec := os.Getenv("MAMMOTH_DELL_E2E")
	if spec == "" {
		t.Skip("MAMMOTH_DELL_E_E2E not set; skipping")
	}
	var host, user, pass string
	if _, err := fmt.Sscanf(spec, "host=%s user=%s pass=%s", &host, &user, &pass); err != nil {
		t.Fatalf("bad spec %q", spec)
	}
	d := New(true, 60*time.Second)
	drives, err := d.PhysicalDrives(context.Background(), host,
		bmc.Credentials{Username: user, Password: pass})
	if err != nil {
		t.Fatalf("PhysicalDrives: %v", err)
	}
	t.Logf("drive pool: %d drives", len(drives))
	for i, dr := range drives {
		t.Logf("  [%d] %s serial=%q cap=%d", i, dr.Name, dr.Serial, dr.SizeBytes)
	}
	if len(drives) == 0 {
		t.Fatal("empty drive pool — the rotating-collection walk failed")
	}
}
