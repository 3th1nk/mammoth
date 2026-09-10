// Hardware RAID volume management (bmc.VolumeCreator /
// bmc.PhysicalDriveEnumerator). Design: standard Redfish first, vendor
// flavor as fallback — verified against Huawei iBMC 6.41 (docs/compat/
// huawei.md):
//   - physical drives live under /Chassis/{id}/Drives with SerialNumber;
//     the controller's Storage resource carries an inline Drives array that
//     additionally exposes Oem.Huawei.DriveID (the integer the creation API
//     wants);
//   - volume creation is a POST to the controller's Volumes collection.
//     The standard payload's RAIDType property is rejected
//     (PropertyUnknown); the accepted shape is
//     {"Name": ..., "Oem": {"Huawei": {"VolumeRaidLevel": "RAID1",
//      "Drives": [<int DriveID>...]}}};
//   - DELETE on a volume resource works (standard);
//   - the controller IGNORES the requested Name and assigns LogicalDriveN —
//     the caller binds via the returned name, not the requested one;
//   - creation/deletion return 202 with a Redfish task to poll.

package redfish

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/stmcginnis/gofish"

	"github.com/3th1nk/mammoth/internal/bmc"
)

type controllerDrive struct {
	ID       string `json:"Id"`
	Model    string `json:"Model"`
	Serial   string `json:"SerialNumber"`
	DriveID  *int   `json:"-"`
	odataID  string
	capacity int64
}

// name mirrors the inventory presentation (Model + Id) so physical drives
// keep stable names across both paths.
func (cd controllerDrive) name() string {
	n := cd.ID
	if cd.Model != "" && !strings.Contains(n, cd.Model) {
		n = cd.Model + " " + n
	}
	return n
}

// PhysicalDrives implements bmc.PhysicalDriveEnumerator: the controller's
// physical drives, with serials — the RAID member pool.
func (d *Driver) PhysicalDrives(ctx context.Context, addr string, cred bmc.Credentials) ([]bmc.DiskView, error) {
	const op = "physical_drives"
	c, err := d.connect(ctx, addr, cred)
	if err != nil {
		return nil, err
	}
	defer c.Logout()

	drives, err := controllerDrives(ctx, c)
	if err != nil {
		return nil, bmc.Classify(op, err)
	}
	out := make([]bmc.DiskView, 0, len(drives))
	for _, dr := range drives {
		out = append(out, bmc.DiskView{
			Name:      dr.name(),
			Serial:    dr.Serial,
			SizeBytes: dr.capacity,
			Protocol:  "raid",
		})
	}
	return out, nil
}

// CreateVolume implements bmc.VolumeCreator. Idempotent by member serials +
// level: a volume already spanning exactly the requested drives is returned
// as-is. The returned name is what the controller assigned (not necessarily
// spec.Name).
func (d *Driver) CreateVolume(ctx context.Context, addr string, cred bmc.Credentials, spec bmc.VolumeSpec) (string, error) {
	const op = "create_volume"
	c, err := d.connect(ctx, addr, cred)
	if err != nil {
		return "", err
	}
	defer c.Logout()

	if len(spec.MemberSerials) == 0 {
		return "", &bmc.Error{Kind: bmc.KindUnsupported, Op: op,
			Detail: "volume has no member drives selected"}
	}

	st, err := firstVolumeStorage(c)
	if err != nil {
		return "", bmc.Classify(op, err)
	}

	drives, err := controllerDrives(ctx, c)
	if err != nil {
		return "", bmc.Classify(op, err)
	}
	bySerial := map[string]controllerDrive{}
	for _, dr := range drives {
		bySerial[dr.Serial] = dr
	}

	// Idempotency: an existing volume spanning exactly the requested serials
	// at the same level is the volume we would create.
	if name, ok := matchingVolume(ctx, c, st, spec, bySerial); ok {
		return name, nil
	}

	volumesURL := strings.TrimSuffix(st.odataID, "/") + "/Volumes"
	driveURLs := make([]string, 0, len(spec.MemberSerials))
	huaweiIDs := make([]int, 0, len(spec.MemberSerials))
	for _, serial := range spec.MemberSerials {
		dr, ok := bySerial[serial]
		if !ok {
			return "", &bmc.Error{Kind: bmc.KindProtocolError, Op: op,
				Detail: fmt.Sprintf("member serial %q not found on the controller", serial)}
		}
		if dr.odataID != "" {
			driveURLs = append(driveURLs, dr.odataID)
		}
		if dr.DriveID != nil {
			huaweiIDs = append(huaweiIDs, *dr.DriveID)
		}
	}

	// Standard Redfish payload first; the vendor flavor when the controller
	// rejects the standard properties (Huawei: PropertyUnknown for RAIDType).
	generic := map[string]any{
		"Name":     spec.Name,
		"RAIDType": spec.RAIDType,
		"Drives":   odataRefs(driveURLs),
	}
	// gofish surfaces non-2xx as an error carrying the response body — the
	// vendor fallback fires on that error OR on a 400 response.
	resp, perr := c.Post(volumesURL, generic)
	if perr == nil {
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode == http.StatusAccepted || (resp.StatusCode >= 200 && resp.StatusCode <= 299) {
			return d.settleVolumeTask(ctx, c, op, string(raw), st)
		}
		perr = fmt.Errorf("%s: %s", resp.Status, string(raw))
	}
	if strings.Contains(perr.Error(), "RAIDType") {
		if len(huaweiIDs) != len(spec.MemberSerials) {
			return "", &bmc.Error{Kind: bmc.KindUnsupported, Op: op,
				Detail: "controller exposes no OEM drive IDs for volume creation"}
		}
		huawei := map[string]any{
			"Name": spec.Name,
			"Oem": map[string]any{"Huawei": map[string]any{
				"VolumeRaidLevel": spec.RAIDType,
				"Drives":          huaweiIDs,
			}},
		}
		return d.postVolumeTask(ctx, c, op, volumesURL, huawei, spec.Name)
	}
	return "", bmc.Classify(op, perr)
}

// postVolumeTask POSTs a vendor-shaped payload and settles the task.
func (d *Driver) postVolumeTask(ctx context.Context, c *gofish.APIClient, op, volumesURL string, payload map[string]any, requested string) (string, error) {
	resp, err := c.Post(volumesURL, payload)
	if err != nil {
		return "", bmc.Classify(op, err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted && (resp.StatusCode < 200 || resp.StatusCode > 299) {
		return "", &bmc.Error{Kind: bmc.KindProtocolError, Op: op,
			Detail: fmt.Sprintf("volume create: %s %s", resp.Status, bmc.FirstLine(string(raw)))}
	}
	st, err := firstVolumeStorage(c)
	if err != nil {
		return "", bmc.Classify(op, err)
	}
	return d.settleVolumeTask(ctx, c, op, string(raw), st)
}

// settleVolumeTask polls a creation task to terminal state and returns the
// name of the volume that appeared (diff against the pre-call collection).
func (d *Driver) settleVolumeTask(ctx context.Context, c *gofish.APIClient, op string, taskRaw string, st *storageRef) (string, error) {
	var task struct {
		Id        string          `json:"Id"`
		TaskState string          `json:"TaskState"`
		Messages  json.RawMessage `json:"Messages"`
	}
	_ = json.Unmarshal([]byte(taskRaw), &task)
	if task.Id != "" {
		if err := pollTask(ctx, c, op, task.Id); err != nil {
			return "", err
		}
	}
	after, err := listVolumeNames(ctx, c, st)
	if err != nil {
		return "", bmc.Classify(op, err)
	}
	if len(after) == 0 {
		return "", &bmc.Error{Kind: bmc.KindProtocolError, Op: op,
			Detail: "volume creation reported success but no volume is present"}
	}
	// The controller auto-names volumes; the newest one is ours (the caller
	// diffs against its own pre-state when it needs to be certain).
	sort.Strings(after)
	return after[len(after)-1], nil
}

// ── storage/drive walking (raw JSON: the Oem fields drive the flow) ────────

type storageRef struct {
	odataID string
}

func firstVolumeStorage(c *gofish.APIClient) (*storageRef, error) {
	sys, err := c.Service.Systems()
	if err != nil {
		return nil, err
	}
	if len(sys) == 0 {
		return nil, fmt.Errorf("no computer system resource")
	}
	storages, err := sys[0].Storage() // follows the advertised link (iBMC: /Storages)
	if err != nil {
		return nil, err
	}
	if len(storages) == 0 {
		return nil, fmt.Errorf("no storage controller reported")
	}
	return &storageRef{odataID: storages[0].ODataID}, nil
}

// controllerDrives walks the storage resource's inline Drives array (which
// carries Oem.Huawei.DriveID) and the referenced drive resources (which
// carry SerialNumber).
func controllerDrives(ctx context.Context, c *gofish.APIClient) ([]controllerDrive, error) {
	st, err := firstVolumeStorage(c)
	if err != nil {
		return nil, err
	}
	raw, err := getRaw(c, st.odataID)
	if err != nil {
		return nil, err
	}
	var s struct {
		Drives []struct {
			ODataID string `json:"@odata.id"`
			Oem     *struct {
				Huawei *struct {
					DriveID *int `json:"DriveID"`
				} `json:"Huawei"`
			} `json:"Oem"`
		} `json:"Drives"`
	}
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, err
	}
	var out []controllerDrive
	for _, entry := range s.Drives {
		dr := controllerDrive{odataID: entry.ODataID}
		if entry.Oem != nil && entry.Oem.Huawei != nil {
			dr.DriveID = entry.Oem.Huawei.DriveID
		}
		full, err := getRaw(c, entry.ODataID)
		if err != nil {
			continue // a dead drive should not sink the enumeration
		}
		var fd struct {
			Id           string `json:"Id"`
			Name         string `json:"Name"`
			Model        string `json:"Model"`
			SerialNumber string `json:"SerialNumber"`
			Capacity     int64  `json:"CapacityBytes"`
		}
		if err := json.Unmarshal(full, &fd); err != nil {
			continue
		}
		dr.ID, dr.Model, dr.Serial, dr.capacity = fd.Id, fd.Model, fd.SerialNumber, fd.Capacity
		out = append(out, dr)
	}
	return out, nil
}

// matchingVolume reports a volume spanning exactly the requested serials at
// the requested level, if one exists.
func matchingVolume(ctx context.Context, c *gofish.APIClient, st *storageRef, spec bmc.VolumeSpec, bySerial map[string]controllerDrive) (string, bool) {
	names, err := listVolumeNames(ctx, c, st)
	if err != nil || len(names) == 0 {
		return "", false
	}
	byID := map[string]controllerDrive{} // drive resource Id → drive
	for _, dr := range bySerial {
		byID[dr.ID] = dr
	}
	for _, name := range names {
		raw, err := getRaw(c, st.odataID+"/Volumes/"+name)
		if err != nil {
			continue
		}
		var v struct {
			RAIDType string `json:"RAIDType"`
			Oem      *struct {
				Huawei *struct {
					VolumeRaidLevel string `json:"VolumeRaidLevel"`
					Spans           []struct {
						Drives []struct {
							ODataID string `json:"@odata.id"`
						} `json:"Drives"`
					} `json:"Spans"`
				} `json:"Huawei"`
			} `json:"Oem"`
		}
		if json.Unmarshal(raw, &v) != nil {
			continue
		}
		level := v.RAIDType
		if v.Oem != nil && v.Oem.Huawei != nil && v.Oem.Huawei.VolumeRaidLevel != "" {
			level = v.Oem.Huawei.VolumeRaidLevel
		}
		if level != spec.RAIDType {
			continue
		}
		got := map[string]bool{}
		if v.Oem != nil && v.Oem.Huawei != nil {
			for _, span := range v.Oem.Huawei.Spans {
				for _, dr := range span.Drives {
					id := dr.ODataID[strings.LastIndex(dr.ODataID, "/")+1:]
					if cd, ok := byID[id]; ok {
						got[cd.Serial] = true
					}
				}
			}
		}
		want := map[string]bool{}
		for _, s := range spec.MemberSerials {
			want[s] = true
		}
		if serialSetsEqual(got, want) {
			return name, true
		}
	}
	return "", false
}

func listVolumeNames(ctx context.Context, c *gofish.APIClient, st *storageRef) ([]string, error) {
	raw, err := getRaw(c, st.odataID+"/Volumes")
	if err != nil {
		return nil, err
	}
	var col struct {
		Members []struct {
			ODataID string `json:"@odata.id"`
		} `json:"Members"`
	}
	if err := json.Unmarshal(raw, &col); err != nil {
		return nil, err
	}
	names := make([]string, 0, len(col.Members))
	for _, m := range col.Members {
		names = append(names, m.ODataID[strings.LastIndex(m.ODataID, "/")+1:])
	}
	return names, nil
}

func odataRefs(urls []string) []map[string]string {
	out := make([]map[string]string, 0, len(urls))
	for _, u := range urls {
		out = append(out, map[string]string{"@odata.id": u})
	}
	return out
}

func serialSetsEqual(a, b map[string]bool) bool {
	if len(a) != len(b) {
		return false
	}
	for k := range a {
		if !b[k] {
			return false
		}
	}
	return true
}


// getRaw fetches one resource as raw JSON.
func getRaw(c *gofish.APIClient, url string) ([]byte, error) {
	resp, err := c.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(resp.Body)
}

// pollTask waits for a Redfish task to leave the running states.
func pollTask(ctx context.Context, c *gofish.APIClient, op, taskID string) error {
	taskURL := "/redfish/v1/TaskService/Tasks/" + taskID
	deadline := time.Now().Add(10 * time.Minute)
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		time.Sleep(2 * time.Second)
		raw, err := getRaw(c, taskURL)
		if err != nil {
			return err
		}
		var task struct {
			TaskState string          `json:"TaskState"`
			Messages  json.RawMessage `json:"Messages"`
		}
		if err := json.Unmarshal(raw, &task); err != nil {
			return &bmc.Error{Kind: bmc.KindProtocolError, Op: op,
				Detail: fmt.Sprintf("task poll decode: %v", err)}
		}
		if task.TaskState != "Running" && task.TaskState != "New" {
			if task.TaskState == "Exception" {
				return &bmc.Error{Kind: bmc.KindProtocolError, Op: op,
					Detail: fmt.Sprintf("volume task exception: %s", bmc.FirstLine(string(task.Messages)))}
			}
			return nil
		}
		if time.Now().After(deadline) {
			return &bmc.Error{Kind: bmc.KindUnreachable, Op: op, Detail: "volume task timed out after 10m"}
		}
	}
}


