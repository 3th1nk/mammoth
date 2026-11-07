package provision

import (
	"encoding/json"
	"testing"

	"github.com/3th1nk/mammoth/internal/store"
)

// The rescue context/report share the install context's JSON keys for the
// media facts (token/media_uri) — the shared cleanup paths parse them
// without knowing the flow — while the report lands under the "rescue" key.
func TestParseRescueContextAndRecord(t *testing.T) {
	ctxJSON := `{"token":"tok1","media_uri":"http://e/boot/boot-tok1.iso","install":{"booted_at":"2026-01-01T00:00:00Z"}}`
	task := &store.Task{Context: json.RawMessage(ctxJSON)}

	rctx := parseRescueContext(task)
	if rctx.Token != "tok1" || rctx.MediaURI == "" {
		t.Fatalf("context parse: %+v", rctx)
	}
	if rec := parseRescueRecord(task); rec != nil {
		t.Fatalf("no rescue record yet, got %+v", rec)
	}

	task.Context = json.RawMessage(`{"token":"tok1","rescue":{"ready_at":"2026-01-01T00:01:00Z","status":"ok","detail":"up"}}`)
	rec := parseRescueRecord(task)
	if rec == nil || rec.Status != "ok" || rec.Detail != "up" {
		t.Fatalf("record parse: %+v", rec)
	}
	if rec.ReadyAt == nil {
		t.Fatal("ready_at must parse")
	}

	// corrupt context: cleanup paths must never fail on it
	task.Context = json.RawMessage(`{broken`)
	if rctx := parseRescueContext(task); rctx.Token != "" {
		t.Fatalf("corrupt context must parse empty, got %+v", rctx)
	}
	if rec := parseRescueRecord(task); rec != nil {
		t.Fatalf("corrupt context must yield no record, got %+v", rec)
	}
}

// The rescue carrier decision: headless machines force PXE (no media slot),
// BMC machines default to the virtual-media mount, boot:"pxe" overrides for
// vMedia-dead firmware, anything else is a schema error.
func TestRescueCarrierFor(t *testing.T) {
	cases := []struct {
		protocol, boot, want string
		wantErr              bool
	}{
		{"redfish", "", "virtual_media", false},
		{"redfish", "virtual_media", "virtual_media", false},
		{"redfish", "pxe", "pxe", false}, // explicit override
		{"ipmi", "", "virtual_media", false},
		{"fake", "virtual_media", "virtual_media", false},
		{"none", "", "pxe", false},              // headless forces PXE
		{"none", "virtual_media", "pxe", false}, // no media slot — forced
		{"none", "pxe", "pxe", false},
		{"redfish", "usb", "", true},
		{"none", "floppy", "", true},
	}
	for _, c := range cases {
		got, err := rescueCarrierFor(c.protocol, c.boot)
		if c.wantErr {
			if err == nil {
				t.Errorf("(%s,%s): want error, got %q", c.protocol, c.boot, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("(%s,%s): unexpected error %v", c.protocol, c.boot, err)
			continue
		}
		if got != c.want {
			t.Errorf("(%s,%s): got %q, want %q", c.protocol, c.boot, got, c.want)
		}
	}
}
