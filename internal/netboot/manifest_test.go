package netboot

import (
	"context"
	"testing"
)

func TestManifestGate(t *testing.T) {
	g := NewManifestGate()

	// Empty gate admits nobody — a dead pool, never an open one.
	if e, err := g.Entry(context.Background(), "aa:bb:cc:dd:ee:01"); err != nil || e != nil {
		t.Fatalf("empty gate Entry = %v, %v; want nil, nil", e, err)
	}
	if g.EnrollEligible("aa:bb:cc:dd:ee:01") {
		t.Fatal("empty gate must not enroll anybody")
	}

	const manifest = `{
	  "armed": [
	    {"mac":"AA-BB-CC-DD-EE-01","kind":"install","task_id":"t1","kernel":"wimboot","initrd":"boot.wim","kernel_args":"quiet","token":"tok-1"}
	  ],
	  "enroll": [{"mac":"AA:BB:CC:DD:EE:02"}]
	}`
	if err := g.Load([]byte(manifest)); err != nil {
		t.Fatalf("load: %v", err)
	}

	// Armed MAC (normalized from dashes): full entry rides Resolver.
	e, err := g.Entry(context.Background(), "aa:bb:cc:dd:ee:01")
	if err != nil || e == nil {
		t.Fatalf("armed entry = %v, %v; want hit", e, err)
	}
	if e.Kind != "install" || e.Kernel != "wimboot" || e.TaskID != "t1" || e.Token != "tok-1" {
		t.Errorf("entry fields drifted: %+v", e)
	}
	// Enroll MAC rides EnrollEligible ONLY — it must never hit Entry (the
	// TFTP renderer / wimboot switch would render artifacts it doesn't own).
	if e2, _ := g.Entry(context.Background(), "aa:bb:cc:dd:ee:02"); e2 != nil {
		t.Errorf("enroll MAC leaked into the armed view: %+v", e2)
	}
	if !g.EnrollEligible("aa:bb:cc:dd:ee:02") {
		t.Error("enroll MAC not eligible")
	}
	if g.EnrollEligible("aa:bb:cc:dd:ee:01") {
		t.Error("armed MAC must not also be enroll-eligible (armed wins by absence, and the manifest has it armed-only)")
	}

	// Unknown MAC: (nil, nil) — the lease gate's reject path.
	if e3, err := g.Entry(context.Background(), "11:22:33:44:55:66"); err != nil || e3 != nil {
		t.Fatalf("unknown MAC = %v, %v; want nil, nil", e3, err)
	}
	if g.EnrollEligible("11:22:33:44:55:66") {
		t.Error("unknown MAC must not enroll")
	}

	// Replace semantics: a new manifest without the armed row closes it.
	if err := g.Load([]byte(`{"armed":[],"enroll":[]}`)); err != nil {
		t.Fatalf("reload: %v", err)
	}
	if e4, _ := g.Entry(context.Background(), "aa:bb:cc:dd:ee:01"); e4 != nil {
		t.Error("reloaded manifest must replace, not merge")
	}
}

// TestManifestGateBrokenLoadKeepsSnapshot pins the load-failure semantics: a
// broken download must not open or close the gate by half.
func TestManifestGateBrokenLoadKeepsSnapshot(t *testing.T) {
	g := NewManifestGate()
	if err := g.Load([]byte(`{"armed":[{"mac":"aa:bb:cc:dd:ee:01","kind":"install"}]}`)); err != nil {
		t.Fatal(err)
	}
	if err := g.Load([]byte(`{"armed": [`)); err == nil {
		t.Fatal("want parse error for truncated manifest")
	}
	if e, _ := g.Entry(context.Background(), "aa:bb:cc:dd:ee:01"); e == nil {
		t.Error("broken load must keep the previous snapshot")
	}
}
