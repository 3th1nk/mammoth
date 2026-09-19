package redfish

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"sort"
	"strings"

	"github.com/3th1nk/mammoth/internal/bmc"
)

// FirmwareInventory lists the controller's firmware (the Redfish
// SoftwareInventory collection under the UpdateService, docs/07-bmc.md §6).
// The walk is the lenient-processor pattern applied to a collection tree:
// raw GETs, anonymous structs with only the consumed fields, per-entry
// tolerance (one broken member never fails the list), sorted output so the
// stored view is stable across reads.
func (d *Driver) FirmwareInventory(ctx context.Context, addr string, cred bmc.Credentials) ([]bmc.FirmwareComponent, error) {
	c, err := d.connect(ctx, addr, cred)
	if err != nil {
		return nil, err
	}

	get := func(url string) ([]byte, error) {
		resp, err := c.Get(url)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		raw, err := io.ReadAll(resp.Body)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode != http.StatusOK {
			return nil, &bmc.Error{Kind: bmc.KindUnreachable, Op: "firmware_inventory",
				Detail: url + ": status " + resp.Status}
		}
		return raw, nil
	}
	return walkFirmware(get)
}

// odataLink is the one shape every collection hop shares.
type odataLink struct {
	ODataID string `json:"@odata.id"`
}

// walkFirmware follows Service root → UpdateService → FirmwareInventory
// collection → members with the fetcher injected (unit-testable against
// canned JSON; the live driver passes the gofish client's GET).
func walkFirmware(get func(string) ([]byte, error)) ([]bmc.FirmwareComponent, error) {
	rootRaw, err := get("/redfish/v1/")
	if err != nil {
		return nil, err
	}
	var root struct {
		UpdateService *odataLink `json:"UpdateService"`
	}
	if json.Unmarshal(rootRaw, &root) != nil || root.UpdateService == nil || root.UpdateService.ODataID == "" {
		return nil, &bmc.Error{Kind: bmc.KindUnsupported, Op: "firmware_inventory",
			Detail: "service root carries no UpdateService link"}
	}
	updateRaw, err := get(root.UpdateService.ODataID)
	if err != nil {
		return nil, err
	}
	var update struct {
		FirmwareInventory *odataLink `json:"FirmwareInventory"`
	}
	if json.Unmarshal(updateRaw, &update) != nil || update.FirmwareInventory == nil || update.FirmwareInventory.ODataID == "" {
		return nil, &bmc.Error{Kind: bmc.KindUnsupported, Op: "firmware_inventory",
			Detail: "UpdateService carries no FirmwareInventory collection"}
	}
	collRaw, err := get(update.FirmwareInventory.ODataID)
	if err != nil {
		return nil, err
	}
	var coll struct {
		Members []odataLink `json:"Members"`
	}
	if json.Unmarshal(collRaw, &coll) != nil {
		return nil, &bmc.Error{Kind: bmc.KindProtocolError, Op: "firmware_inventory",
			Detail: "FirmwareInventory collection undecodable"}
	}

	// lenientFirmware mirrors the lenient-processor discipline: only the
	// consumed fields, no strict typing on the rest (vendors violate the
	// spec in the fields we don't read — that must not fail the entry).
	components := make([]bmc.FirmwareComponent, 0, len(coll.Members))
	for _, m := range coll.Members {
		if m.ODataID == "" {
			continue
		}
		raw, err := get(m.ODataID)
		if err != nil {
			continue // presence without detail beats failing the list
		}
		var item struct {
			ID      string `json:"Id"`
			Name    string `json:"Name"`
			Version string `json:"Version"`
		}
		if json.Unmarshal(raw, &item) != nil || item.ID == "" {
			continue
		}
		components = append(components, bmc.FirmwareComponent{
			ID:      item.ID,
			Name:    item.Name,
			Version: strings.TrimSpace(item.Version),
		})
	}
	sort.Slice(components, func(i, j int) bool {
		if components[i].Name != components[j].Name {
			return components[i].Name < components[j].Name
		}
		return components[i].ID < components[j].ID
	})
	return components, nil
}
