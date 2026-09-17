package redfish

import (
	"testing"

	"github.com/3th1nk/mammoth/internal/bmc"
)

// walkFirmware follows Service root → UpdateService → FirmwareInventory →
// members. The fetcher is injected, so these fixtures script the whole tree.
func TestWalkFirmware(t *testing.T) {
	root := `{"UpdateService":{"@odata.id":"/redfish/v1/UpdateService"}}`
	update := `{"FirmwareInventory":{"@odata.id":"/redfish/v1/UpdateService/FirmwareInventory"}}`
	coll := `{"Members":[
		{"@odata.id":"/redfish/v1/UpdateService/FirmwareInventory/2"},
		{"@odata.id":"/redfish/v1/UpdateService/FirmwareInventory/1"},
		{"@odata.id":""},
		{"@odata.id":"/redfish/v1/UpdateService/FirmwareInventory/broken"}
	]}`
	member1 := `{"Id":"BMC","Name":"iBMC","Version":" 6.41 "}`
	member2 := `{"Id":"BIOS","Name":"BIOS"}` // no version: presence without detail
	// "broken" deliberately serves a spec-violating payload (Version as an
	// object) — the walk must skip it, not fail.

	tree := map[string]string{
		"/redfish/v1/":                                       root,
		"/redfish/v1/UpdateService":                          update,
		"/redfish/v1/UpdateService/FirmwareInventory":        coll,
		"/redfish/v1/UpdateService/FirmwareInventory/1":      member1,
		"/redfish/v1/UpdateService/FirmwareInventory/2":      member2,
		"/redfish/v1/UpdateService/FirmwareInventory/broken": `{"Id":"X","Version":{"bad":true}}`,
	}
	get := func(url string) ([]byte, error) {
		if raw, ok := tree[url]; ok {
			return []byte(raw), nil
		}
		return nil, &bmc.Error{Kind: bmc.KindUnreachable, Op: "firmware_inventory", Detail: url}
	}

	got, err := walkFirmware(get)
	if err != nil {
		t.Fatalf("walkFirmware: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 components (broken member skipped), got %d: %+v", len(got), got)
	}
	// sorted by name
	if got[0].ID != "BIOS" || got[1].ID != "BMC" {
		t.Fatalf("not sorted by name: %+v", got)
	}
	if got[1].Version != "6.41" {
		t.Fatalf("version not trimmed: %q", got[1].Version)
	}
	if got[0].Version != "" {
		t.Fatalf("missing version must stay empty: %+v", got[0])
	}
}

func TestWalkFirmwareMissingLinks(t *testing.T) {
	cases := []struct {
		name string
		tree map[string]string
	}{
		{"no UpdateService", map[string]string{"/redfish/v1/": `{}`}},
		{"no FirmwareInventory collection", map[string]string{
			"/redfish/v1/": `{"UpdateService":{"@odata.id":"/u"}}`,
			"/u":           `{}`,
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			get := func(url string) ([]byte, error) {
				if raw, ok := tc.tree[url]; ok {
					return []byte(raw), nil
				}
				return nil, &bmc.Error{Kind: bmc.KindUnreachable, Op: "firmware_inventory", Detail: url}
			}
			if _, err := walkFirmware(get); err == nil {
				t.Fatalf("expected unsupported error")
			}
		})
	}
}
