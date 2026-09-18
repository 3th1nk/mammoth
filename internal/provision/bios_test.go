package provision

import (
	"strings"
	"testing"
)

// The server-side validation of the two-stage BIOS contract (docs/07-bmc.md
// §6): unknown names are rejected outright, no-ops never reach the BMC, and
// the write set carries only real changes.
func TestBiosDiff(t *testing.T) {
	current := map[string]any{
		"BootMode":    "UEFI",
		"SrIovEnable": false,
		"MaxMem":      float64(64),
	}

	t.Run("unknown names reject with the full list", func(t *testing.T) {
		diff, unknown, noops := biosDiff(current, map[string]any{
			"BootMode":    "Legacy",
			"TypoAttr":    true,
			"AnotherTypo": 1,
		})
		if len(diff) != 1 || diff["BootMode"] != "Legacy" {
			t.Fatalf("diff wrong: %+v", diff)
		}
		if len(unknown) != 2 || unknown[0] != "AnotherTypo" || unknown[1] != "TypoAttr" {
			t.Fatalf("unknown list wrong (sorted): %v", unknown)
		}
		if noops != 0 {
			t.Fatalf("noops = %d", noops)
		}
	})

	t.Run("no-ops never reach the write set", func(t *testing.T) {
		diff, unknown, noops := biosDiff(current, map[string]any{
			"BootMode":    "UEFI", // equal string
			"SrIovEnable": false,  // equal bool
			"MaxMem":      64,     // int vs stored float64 — JSON-equal
		})
		if len(diff) != 0 || len(unknown) != 0 || noops != 3 {
			t.Fatalf("want pure no-op: diff=%v unknown=%v noops=%d", diff, unknown, noops)
		}
	})

	t.Run("type mismatches are real changes, not no-ops", func(t *testing.T) {
		diff, _, _ := biosDiff(current, map[string]any{"SrIovEnable": "false"})
		if len(diff) != 1 {
			t.Fatalf("string \"false\" must not equal bool false: %+v", diff)
		}
		diff, _, _ = biosDiff(current, map[string]any{"MaxMem": true})
		if len(diff) != 1 {
			t.Fatalf("true must not equal 64: %+v", diff)
		}
	})
}

func TestSortedKeys(t *testing.T) {
	got := sortedKeys(map[string]any{"b": 1, "a": 2, "c": 3})
	if strings.Join(got, ",") != "a,b,c" {
		t.Fatalf("sortedKeys = %v", got)
	}
}
