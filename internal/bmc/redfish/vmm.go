package redfish

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/stmcginnis/gofish"
	"github.com/stmcginnis/gofish/redfish"

	"github.com/3th1nk/mammoth/internal/bmc"
)

// vmmControl drives the OEM virtual-media action (VirtualMedia.VmmControl)
// under the CD resource's vendor Oem path: Huawei iBMC /Oem/Huawei/ and
// H3C HDM /Oem/Public/ document the identical payload (docs/compat/
// huawei.md, docs/compat/h3c.md). This firmware family does not advertise
// the standard #VirtualMedia.InsertMedia action; remote media is managed
// exclusively through the OEM action, which accepts nfs:// (and CIFS)
// image URIs — plain HTTP(S) URIs are rejected with
// FileTransferProtocolMismatch (docs/compat/huawei.md §6).
//
// The action returns 202 with a Redfish Task; Connect mounts (BMC fetches
// the image) and Disconnect ejects. Task-level failures (e.g. the BMC cannot
// reach the NFS server) surface as BMC_PROTOCOL_ERROR with the task message.
func (d *Driver) vmmControl(ctx context.Context, c *gofish.APIClient, addr string, imageURI string, action string) error {
	const op = "vmm_control"
	if action != "Connect" && action != "Disconnect" {
		return &bmc.Error{Kind: bmc.KindUnsupported, Op: op,
			Detail: fmt.Sprintf("vmm action %q unsupported", action)}
	}
	if action == "Connect" {
		// Clear the slot first: the firmware rejects a Connect while media is
		// held (iBMC.1.0.ConnectionOccupied). Best effort — a Disconnect on an
		// empty slot fails silently, and a genuinely stuck slot surfaces as a
		// real error from the Connect below.
		_ = d.vmmAction(ctx, c, addr, "", "Disconnect")
		time.Sleep(3 * time.Second)
		// The iBMC's NFS mount is probabilistic: the same request that fails
		// with ConnectionFailed succeeds on an immediate retry (real-hardware
		// capture shows the NFS client completing the full mount dance and
		// reading the image on the winning attempt; the task message's own
		// Resolution is "Please try again"). Retry in-driver so a flaky mount
		// does not burn pipeline task attempts; a fully validated mount takes
		// ~15s (the BMC verifies the ISO9660 metadata after connecting).
		var last error
		for attempt := 0; attempt < 3; attempt++ {
			if attempt > 0 {
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-time.After(4 * time.Second):
				}
			}
			last = d.vmmAction(ctx, c, addr, imageURI, action)
			if last == nil {
				return nil
			}
			// A 400 rejection is a real configuration error (protocol
			// mismatch, bad URI) — fail fast instead of retrying.
			if strings.Contains(last.Error(), "vmm Connect rejected") {
				return last
			}
			// No OEM route at all: retrying cannot discover one.
			var berr *bmc.Error
			if errors.As(last, &berr) && berr.Kind == bmc.KindUnsupported {
				return last
			}
		}
		return last
	}
	return d.vmmAction(ctx, c, addr, imageURI, action)
}

func (d *Driver) vmmAction(ctx context.Context, c *gofish.APIClient, addr string, imageURI string, action string) error {
	const op = "vmm_control"
	managers, err := c.Service.Managers()
	if err != nil {
		return bmc.Classify(op, err)
	}
	if len(managers) == 0 {
		return &bmc.Error{Kind: bmc.KindUnsupported, Op: op, Detail: "no manager resource"}
	}
	// The OEM action hangs off the CD slot's own resource; discover the slot
	// from the VirtualMedia collection instead of hardcoding Manager/CD
	// (manager identities and slot names vary — Managers/1 vs /iBMC).
	payload := map[string]any{"VmmControlType": action}
	if imageURI != "" && action == "Connect" {
		payload["Image"] = imageURI
	}
	// Route candidates: what the slot advertises first, then the known
	// vendor namespaces. A candidate that answers "no such action route"
	// (404/405, or the registry's ActionNotSupported) falls through to
	// the next; any other response means the route received the action.
	var resp *http.Response
	var raw []byte
	for _, target := range vmmControlTargets(c) {
		r, err := c.Post(target, payload)
		if err != nil {
			return bmc.Classify(op, err)
		}
		body, _ := io.ReadAll(r.Body)
		_ = r.Body.Close()
		if routeMiss(r.StatusCode, string(body)) {
			continue
		}
		resp, raw = r, body
		break
	}
	if resp == nil {
		return &bmc.Error{Kind: bmc.KindUnsupported, Op: op,
			Detail: "no OEM VmmControl route on the virtual media slot"}
	}

	if resp.StatusCode == http.StatusBadRequest {
		return &bmc.Error{Kind: bmc.KindProtocolError, Op: op,
			Detail: fmt.Sprintf("vmm %s rejected: %s", action, bmc.FirstLine(string(raw)))}
	}
	if resp.StatusCode != http.StatusAccepted && (resp.StatusCode < 200 || resp.StatusCode > 299) {
		return &bmc.Error{Kind: bmc.KindProtocolError, Op: op,
			Detail: fmt.Sprintf("vmm %s: %s %s", action, resp.Status, bmc.FirstLine(string(raw)))}
	}
	// Poll the mount task to terminal state.
	var task struct {
		Id        string          `json:"Id"`
		TaskState string          `json:"TaskState"`
		Messages  json.RawMessage `json:"Messages"`
	}
	if err := json.Unmarshal(raw, &task); err != nil || task.Id == "" {
		return nil // no task returned: treat as accepted (older firmware)
	}
	taskURL := "/redfish/v1/TaskService/Tasks/" + task.Id
	deadline := time.Now().Add(10 * time.Minute)
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		time.Sleep(2 * time.Second)
		pollResp, err := c.Get(taskURL)
		if err != nil {
			return bmc.Classify(op, err)
		}
		pollRaw, _ := io.ReadAll(pollResp.Body)
		_ = pollResp.Body.Close()
		if pollResp.StatusCode != http.StatusOK {
			return &bmc.Error{Kind: bmc.KindProtocolError, Op: op,
				Detail: fmt.Sprintf("task poll: %s %s", pollResp.Status, bmc.FirstLine(string(pollRaw)))}
		}
		if err := json.Unmarshal(pollRaw, &task); err != nil {
			return &bmc.Error{Kind: bmc.KindProtocolError, Op: op,
				Detail: fmt.Sprintf("task poll decode: %v", err)}
		}
		if taskTerminal(task.TaskState) {
			break
		}
		if time.Now().After(deadline) {
			return &bmc.Error{Kind: bmc.KindUnreachable, Op: op,
				Detail: fmt.Sprintf("vmm %s timed out after 10m", action)}
		}
	}
	if taskFailed(task.TaskState) {
		msg := bmc.FirstLine(string(task.Messages))
		return &bmc.Error{Kind: bmc.KindProtocolError, Op: op,
			Detail: fmt.Sprintf("vmm %s task %s: %s", action, strings.ToLower(task.TaskState), msg)}
	}
	return nil
}

// vmmControlTargets returns the candidate OEM VmmControl action targets
// for the CD slot in try order: what the slot advertises under
// Actions["#VirtualMedia.VmmControl"] first, then the documented vendor
// namespaces — iBMC /Oem/Huawei/ (docs/compat/huawei.md) and HDM
// /Oem/Public/ (docs/compat/h3c.md); the payload is identical, only the
// OEM segment differs.
func vmmControlTargets(c *gofish.APIClient) []string {
	slot := strings.TrimSuffix(cdSlotODataID(c), "/")
	var targets []string
	if raw, err := getRaw(c, slot); err == nil {
		var res struct {
			Actions map[string]struct {
				Target string `json:"target"`
			} `json:"Actions"`
		}
		if json.Unmarshal(raw, &res) == nil {
			if act, ok := res.Actions["#VirtualMedia.VmmControl"]; ok && act.Target != "" {
				targets = append(targets, act.Target)
			}
		}
	}
	for _, ns := range []string{"Oem/Huawei", "Oem/Public"} {
		target := slot + "/" + ns + "/Actions/VirtualMedia.VmmControl"
		dup := false
		for _, t := range targets {
			if t == target {
				dup = true
				break
			}
		}
		if !dup {
			targets = append(targets, target)
		}
	}
	return targets
}

// routeMiss reports whether a POST response means "no such action route"
// rather than "action received and rejected": unknown resource (404),
// method not routable (405), or the Redfish registry's unknown-action
// error (HTTP 400 carrying Base.1.x.ActionNotSupported). Real payload
// rejections carry other codes (PropertyUnknown,
// FileTransferProtocolMismatch, ConnectionOccupied) and do not match.
func routeMiss(status int, body string) bool {
	if status == http.StatusNotFound || status == http.StatusMethodNotAllowed {
		return true
	}
	return status == http.StatusBadRequest && strings.Contains(body, "ActionNotSupported")
}

// Compile-time shape checks against the gofish types used above.
var (
	_ = redfish.VirtualMedia{}
)

// cdSlotODataID locates the CD (or DVD) slot's resource URI across all
// managers; falls back to the traditional "<manager>/VirtualMedia/CD" shape
// when the collection cannot be walked.
func cdSlotODataID(c *gofish.APIClient) string {
	managers, err := c.Service.Managers()
	if err == nil {
		for _, m := range managers {
			vms, verr := m.VirtualMedia()
			if verr != nil {
				continue
			}
			for _, vm := range vms {
				for _, t := range vm.MediaTypes {
					if t == "CD" || t == "DVD" {
						return vm.ODataID
					}
				}
			}
		}
		base := ""
		if len(managers) > 0 {
			base = strings.TrimSuffix(managers[0].ODataID, "/")
		}
		return base + "/VirtualMedia/CD"
	}
	return "/redfish/v1/Managers/1/VirtualMedia/CD"
}
