package provision

import (
	"strings"
	"testing"

	"github.com/3th1nk/mammoth/internal/bmc"
)

// The server-side validation of the two-stage erase contract (docs/07-bmc.md
// §6.2): the requested serials must resolve against the controller's LIVE
// physical-drive table before anything is touched, all=true expands to the
// live table, and duplicates never reach the erase sequence.
func TestResolveEraseTargets(t *testing.T) {
	live := []bmc.DiskView{
		{Name: "Drive 0", Serial: "SER0"},
		{Name: "Drive 1", Serial: "SER1"},
		{Name: "Drive 2", Serial: ""}, // serialless: invisible to erase targeting
	}

	t.Run("explicit serials pass through", func(t *testing.T) {
		got, err := resolveEraseTargets(live, []string{"SER1", "SER0"}, false)
		if err != nil || strings.Join(got, ",") != "SER1,SER0" {
			t.Fatalf("got %v, %v", got, err)
		}
	})

	t.Run("duplicates collapse preserving order", func(t *testing.T) {
		got, err := resolveEraseTargets(live, []string{"SER0", "SER0", "SER1"}, false)
		if err != nil || strings.Join(got, ",") != "SER0,SER1" {
			t.Fatalf("got %v, %v", got, err)
		}
	})

	t.Run("unknown serials reject the whole request", func(t *testing.T) {
		_, err := resolveEraseTargets(live, []string{"SER0", "GHOST"}, false)
		if err == nil || !strings.Contains(err.Error(), "DRIVE_SERIAL_UNKNOWN") || !strings.Contains(err.Error(), "GHOST") {
			t.Fatalf("want DRIVE_SERIAL_UNKNOWN naming GHOST, got %v", err)
		}
		// Nothing-erased semantics: the error is not retryable — a retry
		// would face the same live table.
		if !strings.Contains(err.Error(), "nothing erased") {
			t.Fatalf("error must state nothing was erased: %v", err)
		}
	})

	t.Run("all expands to the live table only", func(t *testing.T) {
		got, err := resolveEraseTargets(live, nil, true)
		if err != nil || strings.Join(got, ",") != "SER0,SER1" {
			t.Fatalf("serialless drive must not enter the set: %v, %v", got, err)
		}
	})

	t.Run("all on an empty table is a schema error", func(t *testing.T) {
		_, err := resolveEraseTargets(nil, nil, true)
		if err == nil || !strings.Contains(err.Error(), "SCHEMA_INVALID_ACTION") {
			t.Fatalf("want SCHEMA_INVALID_ACTION, got %v", err)
		}
	})

	t.Run("no selection at all is a schema error", func(t *testing.T) {
		_, err := resolveEraseTargets(live, nil, false)
		if err == nil || !strings.Contains(err.Error(), "SCHEMA_INVALID_ACTION") {
			t.Fatalf("want SCHEMA_INVALID_ACTION, got %v", err)
		}
	})
}
