// Secure drive erase (bmc.DriveEraser — NIST 800-88 media sanitization at
// the controller level, docs/07-bmc.md §6.2). The standard Redfish shape is
// a POST to the drive resource's #Drive.SecureErase action target; many
// controllers answer 202 with a task. What the controller actually does
// (ATA sanitize, NVMe crypto erase, overwrite passes) is vendor policy —
// the driver reports what the responses say and does not pretend to know.

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

// SecureErase implements bmc.DriveEraser: erase each requested drive via
// its #Drive.SecureErase action, sequentially (the Registry already
// serializes per address — BMCs are weak hardware). All serials resolve
// before the first erase starts; one unknown serial aborts the request.
func (d *Driver) SecureErase(ctx context.Context, addr string, cred bmc.Credentials, serials []string) ([]bmc.SanitizeResult, error) {
	const op = "secure_erase"
	if len(serials) == 0 {
		return nil, &bmc.Error{Kind: bmc.KindProtocolError, Op: op,
			Detail: "no drives requested"}
	}
	c, err := d.connect(ctx, addr, cred)
	if err != nil {
		return nil, err
	}

	drives, err := controllerDrives(ctx, c)
	if err != nil {
		return nil, d.classify(op, err)
	}
	bySerial := map[string]controllerDrive{}
	for _, dr := range drives {
		bySerial[dr.Serial] = dr
	}
	targets := make([]controllerDrive, 0, len(serials))
	for _, serial := range serials {
		dr, ok := bySerial[serial]
		if !ok {
			return nil, &bmc.Error{Kind: bmc.KindProtocolError, Op: op,
				Detail: fmt.Sprintf("serial %q not found on the controller — nothing erased", serial)}
		}
		targets = append(targets, dr)
	}

	results := make([]bmc.SanitizeResult, 0, len(targets))
	for _, dr := range targets {
		res, err := eraseOneDrive(ctx, c, op, dr)
		if err != nil {
			return results, err
		}
		results = append(results, res)
	}
	return results, nil
}

// eraseOneDrive POSTs one drive's SecureErase action and settles the task.
func eraseOneDrive(ctx context.Context, c *gofish.APIClient, op string, dr controllerDrive) (bmc.SanitizeResult, error) {
	res := bmc.SanitizeResult{Serial: dr.Serial}
	if dr.odataID == "" {
		return res, &bmc.Error{Kind: bmc.KindProtocolError, Op: op,
			Detail: fmt.Sprintf("drive %q exposes no resource URI", dr.Serial)}
	}
	raw, err := getRaw(c, dr.odataID)
	if err != nil {
		return res, bmc.Classify(op, err)
	}
	target, err := secureEraseTarget(raw)
	if err != nil {
		return res, err
	}
	resp, err := c.Post(target, map[string]any{})
	if err != nil {
		return res, bmc.Classify(op, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		if kind := bmc.HttpStatusKind(op, resp.StatusCode, string(body)); kind != nil {
			return res, kind
		}
		return res, &bmc.Error{Kind: bmc.KindProtocolError, Op: op,
			Detail: fmt.Sprintf("secure erase %s: %s %s", dr.Serial, resp.Status, bmc.FirstLine(string(body)))}
	}
	// Vendors vary in what they report with the acceptance; everything here
	// is best-effort decoration on a successful erase.
	if resp.StatusCode == http.StatusAccepted {
		var task struct {
			TaskState string `json:"TaskState"`
			ID        string `json:"Id"`
			StartTime string `json:"StartTime"`
			EndTime   string `json:"EndTime"`
			Task      *struct {
				ODataID string `json:"@odata.id"`
			} `json:"Task"`
			ODataID string `json:"@odata.id"`
		}
		if json.Unmarshal(body, &task) == nil {
			res.StartedAt, res.EndedAt = task.StartTime, task.EndTime
			switch {
			case task.TaskState != "" && taskTerminal(task.TaskState):
				if taskFailed(task.TaskState) {
					return res, &bmc.Error{Kind: bmc.KindProtocolError, Op: op,
						Detail: fmt.Sprintf("secure erase task for %s ended in %s", dr.Serial, task.TaskState)}
				}
			case task.Task != nil && task.Task.ODataID != "":
				return res, pollTask(ctx, c, op, taskIDFromURI(task.Task.ODataID))
			case task.ID != "" && (task.ODataID == "" || strings.Contains(task.ODataID, "Tasks")):
				return res, pollTask(ctx, c, op, task.ID)
			}
		}
	}
	return res, nil
}

// secureEraseTarget reads the drive resource's action target. A drive
// without the action is a hard unsupported: the controller cannot purge
// that drive and the caller must know (NIST 800-88 demands a method).
func secureEraseTarget(driveRaw []byte) (string, error) {
	var drive struct {
		Actions map[string]struct {
			Target string `json:"target"`
		} `json:"Actions"`
	}
	if json.Unmarshal(driveRaw, &drive) != nil {
		return "", &bmc.Error{Kind: bmc.KindProtocolError, Op: "secure_erase",
			Detail: "drive resource undecodable"}
	}
	if act, ok := drive.Actions["#Drive.SecureErase"]; ok && act.Target != "" {
		return act.Target, nil
	}
	return "", &bmc.Error{Kind: bmc.KindUnsupported, Op: "secure_erase",
		Detail: "drive declares no #Drive.SecureErase action — this controller cannot purge it"}
}
