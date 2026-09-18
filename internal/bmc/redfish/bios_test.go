package redfish

import (
	"testing"

	"github.com/3th1nk/mammoth/internal/bmc"
)

func biosTestTree() map[string]string {
	return map[string]string{
		"/redfish/v1/Systems":   `{"Members":[{"@odata.id":"/redfish/v1/Systems/1"}]}`,
		"/redfish/v1/Systems/1": `{"Bios":{"@odata.id":"/redfish/v1/Systems/1/Bios"}}`,
		"/redfish/v1/Systems/1/Bios": `{"Attributes":{"BootMode":"UEFI","SrIovEnable":false},
			"@Redfish.Settings":{"SettingsObject":{"@odata.id":"/redfish/v1/Systems/1/Bios/SD"}}}`,
	}
}

func TestParseBiosAttributes(t *testing.T) {
	attrs, err := parseBiosAttributes([]byte(`{"Attributes":{"BootMode":"UEFI","SrIovEnable":false,"MaxMem":64}}`))
	if err != nil {
		t.Fatalf("parseBiosAttributes: %v", err)
	}
	if attrs["BootMode"] != "UEFI" || attrs["SrIovEnable"] != false || attrs["MaxMem"] != float64(64) {
		t.Fatalf("vendor JSON types must survive: %+v", attrs)
	}
	if _, err := parseBiosAttributes([]byte(`{}`)); err == nil {
		t.Fatalf("missing Attributes table must be a protocol error")
	}
}

func TestSettingsObjectURI(t *testing.T) {
	uri, err := settingsObjectURI([]byte(`{"@Redfish.Settings":{"SettingsObject":{"@odata.id":"/redfish/v1/Systems/1/Bios/SD"}}}`))
	if err != nil || uri != "/redfish/v1/Systems/1/Bios/SD" {
		t.Fatalf("settings URI = %q, %v", uri, err)
	}
	if _, err := settingsObjectURI([]byte(`{"Attributes":{}}`)); err == nil {
		t.Fatalf("Bios without @Redfish.Settings must be unsupported (write side)")
	}
}

func TestBiosResourceLocation(t *testing.T) {
	tree := biosTestTree()
	get := func(url string) ([]byte, error) {
		if raw, ok := tree[url]; ok {
			return []byte(raw), nil
		}
		return nil, &bmc.Error{Kind: bmc.KindUnreachable, Op: "bios_attributes", Detail: url}
	}
	raw, err := biosResource(get)
	if err != nil {
		t.Fatalf("biosResource: %v", err)
	}
	attrs, err := parseBiosAttributes(raw)
	if err != nil || attrs["BootMode"] != "UEFI" {
		t.Fatalf("read path broken: %v %+v", err, attrs)
	}

	// No Bios link on the system: fall back to the canonical sibling path.
	delete(tree, "/redfish/v1/Systems/1")
	tree["/redfish/v1/Systems/1"] = `{}`
	tree["/redfish/v1/Systems/1/Bios"] = tree["/redfish/v1/Systems/1/Bios"]
	if _, err := biosResource(get); err != nil {
		t.Fatalf("canonical fallback path must work: %v", err)
	}

	// No systems at all: unsupported, not a crash.
	tree["/redfish/v1/Systems"] = `{"Members":[]}`
	if _, err := biosResource(get); err == nil {
		t.Fatalf("empty system collection must be unsupported")
	}
}
