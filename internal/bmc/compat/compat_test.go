package compat

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLookupAndModelRegex(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("supermicro.yaml", `
vendor: SuperMicro            # normalization: case-insensitive match
models:
  - "X12DP.*"
partial_inventory:
  - "older BIOS: drives behind HBA not enumerated"
one_shot_boot: false
`)
	write("dell.yaml", `
vendor: dell
partial_inventory:
  - "legacy iDRAC: no Storage collection"
one_shot_boot: true
`)

	r, err := LoadDir(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	if e := r.Lookup("Supermicro", "X12DPi-T8"); e == nil || len(e.PartialInventory) != 1 {
		t.Fatalf("vendor+model match failed: %+v", e)
	}
	if e := r.Lookup("supermicro", "X13DPG"); e != nil {
		t.Fatalf("model regex must anchor: matched %+v", e)
	}
	if e := r.Lookup("Dell", "PowerEdge R750"); e == nil || e.OneShotBoot == nil {
		t.Fatalf("dell lookup / declared flag failed: %+v", e)
	}
	if supported, declared := r.OneShotBoot("dell", "R750"); !supported || !declared {
		t.Fatalf("one_shot_boot: true must surface as supported+declared")
	}
	if supported, declared := r.OneShotBoot("unknown-vendor", "m"); !supported || declared {
		t.Fatalf("undeclared vendors must default to supported, not declared")
	}
	if notes := r.InventoryNotes("acme", "anything"); notes != nil {
		t.Fatalf("unknown vendor must have no notes: %v", notes)
	}
}

func TestParseRejectsEntryWithoutVendor(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "bad.yaml"), []byte("models: [x]"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadDir(dir); err == nil {
		t.Fatal("entry without vendor must fail to load")
	}
}
