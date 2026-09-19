package redfish

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/stmcginnis/gofish"

	"github.com/3th1nk/mammoth/internal/bmc"
)

// BiosSetter speaks the Redfish Bios resource: the vendor-neutral attribute
// table under the computer system, changed by PATCHing the settings object
// (@Redfish.Settings.SettingsObject — the "SD" resource). Writes are
// PENDING by protocol semantics: the BMC applies them at the next boot.
// The parsing helpers are pure and unit-tested in bios_test.go; the driver
// methods wire them to the live client with the same ETag/If-Match care the
// boot-override PATCH proved necessary on iBMC.

// BiosAttributes returns the current attribute table.
func (d *Driver) BiosAttributes(ctx context.Context, addr string, cred bmc.Credentials) (map[string]any, error) {
	c, err := d.connect(ctx, addr, cred)
	if err != nil {
		return nil, err
	}

	get := func(url string) ([]byte, error) { return getRaw(c, url) }
	raw, err := biosResource(get)
	if err != nil {
		return nil, err
	}
	return parseBiosAttributes(raw)
}

// SetBiosAttributes PATCHes the settings object with the requested pending
// values (applied at the next boot). A 202 carries a task reference, polled
// with the same bounded, exception-aware poll volume creation uses.
func (d *Driver) SetBiosAttributes(ctx context.Context, addr string, cred bmc.Credentials, attrs map[string]any) error {
	const op = "set_bios_attributes"
	if len(attrs) == 0 {
		return &bmc.Error{Kind: bmc.KindProtocolError, Op: op,
			Detail: "no attributes requested"}
	}
	c, err := d.connect(ctx, addr, cred)
	if err != nil {
		return err
	}

	get := func(url string) ([]byte, error) { return getRaw(c, url) }
	biosRaw, err := biosResource(get)
	if err != nil {
		return err
	}
	settings, err := settingsObjectURI(biosRaw)
	if err != nil {
		return err
	}

	// The settings resource requires If-Match with its own CURRENT ETag
	// (iBMC 6.41: the value advertised on the main Bios resource's
	// @Redfish.Settings annotation goes stale — PATCH with it fails 412).
	// Content-Type must be set explicitly: with custom headers gofish does
	// not add it and iBMC answers 400 MalformedJSON to an untyped body.
	// The pending resource starts empty and accepts a sparse delta (verified
	// against iBMC 6.41: PATCH /Bios/Settings → 200, applied at next boot).
	headers := map[string]string{"Content-Type": "application/json"}
	settingsRaw := []byte{}
	if resp, gerr := c.Get(settings); gerr == nil {
		raw, rerr := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if rerr == nil {
			settingsRaw = raw
		}
		if etag := resp.Header.Get("ETag"); etag != "" {
			headers["If-Match"] = etag
		}
	}
	// Merge into pending: the SD resource carries pending values that a
	// blind PATCH would replace wholesale — read-modify-write keeps any
	// earlier pending entries intact.
	pending := map[string]any{}
	var sd struct {
		Attributes map[string]any `json:"Attributes"`
	}
	if json.Unmarshal(settingsRaw, &sd) == nil {
		pending = sd.Attributes
	}
	if pending == nil {
		pending = map[string]any{}
	}
	for k, v := range attrs {
		pending[k] = v
	}
	payload, err := json.Marshal(map[string]any{"Attributes": pending})
	if err != nil {
		return err
	}

	resp, err := c.PatchWithHeaders(settings, payload, headers)
	if err != nil {
		return d.classify(op, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(resp.Body)
		if kind := bmc.HttpStatusKind(op, resp.StatusCode, string(raw)); kind != nil {
			return kind
		}
		return &bmc.Error{Kind: bmc.KindProtocolError, Op: op,
			Detail: fmt.Sprintf("settings PATCH: %s %s", resp.Status, bmc.FirstLine(string(raw)))}
	}
	if resp.StatusCode == http.StatusAccepted {
		return pollSettingsTask(ctx, c, op, resp)
	}
	return nil // 200/204: accepted synchronously
}

// pollSettingsTask resolves a 202: an inline task state done in place, a
// task reference to poll, or (vendor tolerance) neither — accept, the
// pending values land at the next boot.
func pollSettingsTask(ctx context.Context, c *gofish.APIClient, op string, resp *http.Response) error {
	raw, _ := io.ReadAll(resp.Body)
	var body struct {
		TaskState string `json:"TaskState"`
		ID        string `json:"Id"`
		ODataID   string `json:"@odata.id"`
		Task      *struct {
			ODataID string `json:"@odata.id"`
		} `json:"Task"`
	}
	_ = json.Unmarshal(raw, &body)
	switch {
	case body.TaskState == "Exception":
		return &bmc.Error{Kind: bmc.KindProtocolError, Op: op,
			Detail: fmt.Sprintf("settings task exception: %s", bmc.FirstLine(string(raw)))}
	case body.TaskState == "New" || body.TaskState == "Running":
		return pollTask(ctx, c, op, body.ID)
	case body.Task != nil && body.Task.ODataID != "":
		return pollTask(ctx, c, op, taskIDFromURI(body.Task.ODataID))
	case body.ID != "" && body.ODataID != "" && strings.Contains(body.ODataID, "Tasks"):
		return pollTask(ctx, c, op, body.ID)
	default:
		return nil
	}
}

// taskIDFromURI extracts the trailing task id from a task @odata.id
// ("/redfish/v1/TaskService/Tasks/123" → "123").
func taskIDFromURI(uri string) string {
	if i := strings.LastIndexByte(uri, '/'); i >= 0 {
		return uri[i+1:]
	}
	return uri
}

// biosResource locates and fetches the Bios resource off the first computer
// system: the system's own Bios link when present, the canonical sibling
// path otherwise.
func biosResource(get func(string) ([]byte, error)) ([]byte, error) {
	const op = "bios_attributes"
	systemsRaw, err := get("/redfish/v1/Systems")
	if err != nil {
		return nil, err
	}
	var systems struct {
		Members []struct {
			ODataID string `json:"@odata.id"`
		} `json:"Members"`
	}
	if json.Unmarshal(systemsRaw, &systems) != nil || len(systems.Members) == 0 {
		return nil, &bmc.Error{Kind: bmc.KindUnsupported, Op: op,
			Detail: "no computer systems enumerable"}
	}
	sysURI := systems.Members[0].ODataID
	sysRaw, err := get(sysURI)
	if err != nil {
		return nil, err
	}
	var sys struct {
		Bios *struct {
			ODataID string `json:"@odata.id"`
		} `json:"Bios"`
	}
	if json.Unmarshal(sysRaw, &sys) != nil {
		return nil, &bmc.Error{Kind: bmc.KindProtocolError, Op: op,
			Detail: "computer system undecodable"}
	}
	uri := sysURI + "/Bios"
	if sys.Bios != nil && sys.Bios.ODataID != "" {
		uri = sys.Bios.ODataID
	}
	return get(uri)
}

// parseBiosAttributes extracts the attribute table (lenient: only the
// consumed field).
func parseBiosAttributes(raw []byte) (map[string]any, error) {
	const op = "bios_attributes"
	var bios struct {
		Attributes map[string]any `json:"Attributes"`
	}
	if json.Unmarshal(raw, &bios) != nil || bios.Attributes == nil {
		return nil, &bmc.Error{Kind: bmc.KindProtocolError, Op: op,
			Detail: "Bios resource carries no Attributes table"}
	}
	return bios.Attributes, nil
}

// settingsObjectURI reads the @Redfish.Settings annotation for the write
// target — the vendor's pending-values resource (commonly ".../Bios/SD").
func settingsObjectURI(biosRaw []byte) (string, error) {
	const op = "set_bios_attributes"
	var bios struct {
		Settings struct {
			SettingsObject *struct {
				ODataID string `json:"@odata.id"`
			} `json:"SettingsObject"`
		} `json:"@Redfish.Settings"`
	}
	if json.Unmarshal(biosRaw, &bios) != nil || bios.Settings.SettingsObject == nil ||
		bios.Settings.SettingsObject.ODataID == "" {
		return "", &bmc.Error{Kind: bmc.KindUnsupported, Op: op,
			Detail: "Bios resource declares no @Redfish.Settings settings object — attribute writes are not supported by this controller"}
	}
	return bios.Settings.SettingsObject.ODataID, nil
}
