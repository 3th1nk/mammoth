package redfish

// Real-hardware verification of the multi-controller storage walk, gated by
// MAMMOTH_DELL_E2E='host=... user=... pass=...': PhysicalDrives must return
// the PERC's pool regardless of the Storage collection's Members order
// (docs/compat/dell.md §2 — the iDRAC8 order instability that broke the old
// first-controller pick).

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/3th1nk/mammoth/internal/bmc"
)

func e2eCreds(t *testing.T) (host, user, pass string) {
	t.Helper()
	spec := os.Getenv("MAMMOTH_DELL_E2E")
	if spec == "" {
		t.Skip("MAMMOTH_DELL_E2E not set; skipping real-iDRAC PhysicalDrives check")
	}
	var h, u, p string
	if _, err := fmt.Sscanf(spec, "host=%s user=%s pass=%s", &h, &u, &p); err != nil {
		t.Fatalf("bad spec %q: %v", spec, err)
	}
	return h, u, p
}

func TestE2EPhysicalDrives(t *testing.T) {
	host, user, pass := e2eCreds(t)
	d := New(true, 60*time.Second)
	drives, err := d.PhysicalDrives(context.Background(), host,
		bmc.Credentials{Username: user, Password: pass})
	if err != nil {
		t.Fatalf("PhysicalDrives: %v", err)
	}
	t.Logf("drive pool: %d drives", len(drives))
	for i, dr := range drives {
		t.Logf("  [%d] %s %s serial=%q cap=%d", i, dr.Name, dr.Medium, dr.Serial, dr.SizeBytes)
	}
	if len(drives) == 0 {
		t.Fatal("empty drive pool — the rotating-collection walk failed")
	}
	for _, dr := range drives {
		if dr.SizeBytes == 0 {
			t.Errorf("drive %q has no capacity", dr.Name)
		}
	}
}
