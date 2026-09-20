package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/3th1nk/mammoth/internal/api/gen"
	"github.com/3th1nk/mammoth/internal/obs"
	"github.com/3th1nk/mammoth/internal/provision"
	"github.com/3th1nk/mammoth/internal/render"
	"github.com/3th1nk/mammoth/internal/store"
	"github.com/3th1nk/mammoth/internal/store/queue"
)

// ── actions & jobs ──────────────────────────────────────────────────────────

// PerformMachineAction: the unified single-machine entry (docs/03-api.md §2).
// All BMC actions return 202 + job — there is no synchronous BMC call
// endpoint, by design.
func (s *Server) PerformMachineAction(ctx context.Context, request gen.PerformMachineActionRequestObject) (gen.PerformMachineActionResponseObject, error) {
	machine, err := s.Machines.Get(ctx, string(request.Id))
	if err != nil {
		return nil, err
	}
	body := request.Body
	if body == nil {
		return nil, verr("SCHEMA_INVALID_ACTION", "action body required")
	}
	actionRaw, aType, err := encodeAction(body)
	if err != nil {
		return nil, err
	}
	if err := s.validateBiosConfirm(actionRaw, aType); err != nil {
		return nil, err
	}
	if err := s.validateEraseConfirm(actionRaw, aType); err != nil {
		return nil, err
	}
	jobType := "power"
	flow := provision.FlowPower
	if aType == "discover" {
		jobType = "discover"
		flow = provision.FlowDiscover
	}

	job, err := s.createJobRecord(ctx, createJobRecord{
		jobType:        jobType,
		flow:           flow,
		machineIDs:     []string{machine.ID},
		actionRaw:      actionRaw,
		requestRaw:     mustJSON(body),
		idempotencyKey: derefOr(request.Params.IdempotencyKey, ""),
		createdBy:      s.subject(ctx),
	})
	if err != nil {
		return nil, err
	}
	return gen.PerformMachineAction202JSONResponse(*job), nil
}

// CreateJob: batch submission (install spec / power batch / discover batch).
func (s *Server) CreateJob(ctx context.Context, request gen.CreateJobRequestObject) (gen.CreateJobResponseObject, error) {
	body := request.Body
	if body == nil || len(body.Targets.MachineIds) == 0 {
		return nil, verr("SCHEMA_INVALID_JOB", "targets.machine_ids must not be empty")
	}
	machineIDs := make([]string, 0, len(body.Targets.MachineIds))
	for _, id := range body.Targets.MachineIds {
		machineIDs = append(machineIDs, string(id))
	}
	if err := s.verifyMachinesExist(ctx, machineIDs); err != nil {
		return nil, err
	}

	var specRaw json.RawMessage
	if body.Spec != nil {
		b, err := json.Marshal(body.Spec)
		if err != nil {
			return nil, verr("SCHEMA_INVALID_SPEC", "spec is not valid: %v", err)
		}
		specRaw = b
	}

	var flow string
	var actionRaw json.RawMessage
	switch body.Type {
	case "power":
		flow = provision.FlowPower
		if body.Action == nil {
			return nil, verr("SCHEMA_INVALID_JOB", "power jobs require an action")
		}
		var err error
		actionRaw, _, err = encodeAction(body.Action)
		if err != nil {
			return nil, err
		}
	case "discover":
		flow = provision.FlowDiscover
		if body.Action != nil {
			var err error
			actionRaw, _, err = encodeAction(body.Action)
			if err != nil {
				return nil, err
			}
		} else {
			actionRaw = mustJSON(map[string]any{"type": "discover", "probe": "auto"})
		}
	case "install":
		flow = provision.FlowInstall
		if body.Spec == nil {
			return nil, verr("SCHEMA_INVALID_JOB", "install jobs require a spec")
		}
		if err := validateInstallSpec(body.Spec); err != nil {
			return nil, err
		}
		if err := s.validateBootStrategy(specRaw); err != nil {
			return nil, err
		}
	default:
		return nil, verr("SCHEMA_INVALID_JOB", "unknown job type %q", body.Type)
	}

	policy := store.Policy{OnTaskFailure: "continue"}
	if body.Policy != nil {
		if body.Policy.Concurrency != nil {
			policy.Concurrency = *body.Policy.Concurrency
		}
		if body.Policy.OnTaskFailure != nil {
			policy.OnTaskFailure = string(*body.Policy.OnTaskFailure)
		}
		if body.Policy.VerifyLayout != nil {
			policy.VerifyLayout = body.Policy.VerifyLayout
		}
		if body.Policy.TaskTimeoutSeconds != nil {
			policy.TaskTimeoutSeconds = *body.Policy.TaskTimeoutSeconds
		}
	}

	var hostnamePattern *string
	if body.Type == "install" && body.Spec != nil && body.Spec.Identity != nil {
		hostnamePattern = body.Spec.Identity.HostnamePattern
	}
	// Base-spec keep usage is a request-level error: it applies to every
	// target, so a partial/none distro rejects the whole submission (docs/06
	// §5). Per-machine override violations stay task-level (pre-failed).
	if body.Type == "install" && body.Spec != nil && body.Spec.Storage != nil {
		usesKeepDisk, usesKeepParts := false, false
		for _, disk := range body.Spec.Storage.Disks {
			switch derefOr(disk.Keep, "") {
			case "disk":
				usesKeepDisk = true
			case "partitions":
				usesKeepParts = true
			}
			if len(derefOr(disk.Preserve, nil)) > 0 {
				usesKeepParts = true
			}
		}
		if usesKeepDisk || usesKeepParts {
			driver, derr := s.Render.For(body.Spec.Image.Distro)
			if derr == nil {
				switch driver.KeepPartitionSupport() {
				case render.SupportPartial:
					if usesKeepParts {
						return nil, verr("SCHEMA_UNSUPPORTED_KEEP",
							"distro %s declares partial keep support: keep: disk only; keep: partitions/preserve is not supported",
							body.Spec.Image.Distro)
					}
				case render.SupportNone:
					return nil, verr("SCHEMA_UNSUPPORTED_KEEP",
						"distro %s does not support keep semantics", body.Spec.Image.Distro)
				}
			}
		}
	}

	job, err := s.createJobRecord(ctx, createJobRecord{
		jobType:         string(body.Type),
		flow:            flow,
		machineIDs:      machineIDs,
		actionRaw:       actionRaw,
		specRaw:         specRaw,
		policy:          policy,
		requestRaw:      mustJSON(body),
		idempotencyKey:  derefOr(request.Params.IdempotencyKey, ""),
		createdBy:       s.subject(ctx),
		hostnamePattern: hostnamePattern,
		overrides:       derefOr(body.Targets.Overrides, nil),
	})
	if err != nil {
		return nil, err
	}
	return gen.CreateJob202JSONResponse(*job), nil
}

// createJobRecord persists job + tasks + stages in one transaction and
// enqueues one message per task. Idempotent replays return the original job.
func (s *Server) createJobRecord(ctx context.Context, in createJobRecord) (*gen.Job, error) {
	if in.idempotencyKey != "" {
		existing, err := s.Jobs.GetByIdempotencyKey(ctx, in.idempotencyKey)
		if err != nil {
			return nil, err
		}
		if existing != nil {
			ids, _ := s.Jobs.MachineIDs(ctx, existing.ID)
			return ptrJob(jobOut(existing, ids)), nil
		}
	}

	job := &store.Job{
		ID:           store.NewID("job"),
		Type:         in.jobType,
		Request:      in.requestRaw,
		Action:       in.actionRaw,
		SpecResolved: in.specRaw,
		Policy:       in.policy,
		State:        "pending",
		CreatedBy:    in.createdBy,
	}
	if in.idempotencyKey != "" {
		job.IdempotencyKey = str(in.idempotencyKey)
	}

	taskIDs := make([]string, 0, len(in.machineIDs))
	var taskContexts []json.RawMessage
	var taskInitials []store.TaskInit
	if in.jobType == "install" || in.jobType == "discover" {
		// Install and discover tasks get their machine-facing credential
		// (token — the installer/probe cannot hold a bearer token) and the
		// install-expanded hostname/spec at creation; context grows after.
		pattern := ""
		if in.hostnamePattern != nil {
			pattern = *in.hostnamePattern
		}
		for i, mid := range in.machineIDs {
			taskIDs = append(taskIDs, store.NewID("tsk"))
			token, terr := newToken()
			if terr != nil {
				return nil, terr
			}
			tctx := map[string]any{"token": token}
			if pattern != "" {
				tctx["hostname"] = strings.ReplaceAll(pattern, "{index}", itoa(i+1))
			}
			merged := mergeSpecOverride(in.specRaw, in.overrides, mid)
			if merged != nil {
				tctx["spec"] = json.RawMessage(merged)
			}

			// Submit-side snapshot binding (docs/04-install-spec.md §5.1):
			// a spec that keeps partitions must hit the machine's latest
			// layout snapshot, or the task is marked failed at creation —
			// without blocking sibling machines.
			effective := in.specRaw
			if merged != nil {
				effective = merged
			}
			init := store.TaskInit{}
			if verr := s.validateDistroKeepSupport(effective); verr != nil {
				init.State = "failed"
				init.Error = &store.ErrorInfo{
					Code:      appErrCode(verr),
					Message:   verr.Error(),
					Retryable: false,
				}
			} else if verr := s.validateBootStrategy(effective); verr != nil {
				init.State = "failed"
				init.Error = &store.ErrorInfo{
					Code:      appErrCode(verr),
					Message:   verr.Error(),
					Retryable: false,
				}
			} else if verr := s.validateFirmwareMatch(ctx, mid, effective); verr != nil {
				init.State = "failed"
				init.Error = &store.ErrorInfo{
					Code:      appErrCode(verr),
					Message:   verr.Error(),
					Retryable: false,
				}
			} else if verr := s.validateSnapshotBinding(ctx, mid, effective); verr != nil {
				init.State = "failed"
				init.Error = &store.ErrorInfo{
					Code:      appErrCode(verr),
					Message:   verr.Error(),
					Retryable: false,
				}
			}
			taskInitials = append(taskInitials, init)

			b, _ := json.Marshal(tctx)
			taskContexts = append(taskContexts, b)
		}
	}
	for len(taskIDs) < len(in.machineIDs) {
		taskIDs = append(taskIDs, store.NewID("tsk"))
	}
	input := store.CreateJobInput{
		Job:          job,
		TaskIDs:      taskIDs,
		MachineIDs:   in.machineIDs,
		Stages:       provision.StageNames(in.flow),
		TaskContexts: taskContexts,
		TaskInitials: taskInitials,
	}
	if err := s.Jobs.CreateJobWithTasks(ctx, input); err != nil {
		return nil, err
	}

	// Enqueue one message per task (pre-failed tasks are not queued); span
	// context rides along so the task stays one trace across facets.
	for i, tid := range taskIDs {
		if i < len(taskInitials) && taskInitials[i].State == "failed" {
			continue
		}
		msg := provision.MarshalMessage(ctx, tid)
		if err := s.Queue.Enqueue(ctx, provision.QueueTasks, msg, queue.EnqueueOptions{}); err != nil {
			// The job stays pending; the reaper/runner loop of record picks
			// enqueued tasks up, and a partially enqueued batch completes as
			// the queue drains. Surface the enqueue failure to the client.
			obs.FromContext(ctx).ErrorContext(ctx, "enqueue failed",
				obs.FieldJobID, job.ID, obs.FieldTaskID, tid, "err", err.Error())
			return nil, verr("JOB_ENQUEUE_FAILED", "task enqueue failed: %v", err)
		}
		_ = i
	}

	s.Events.Append(ctx, "job", job.ID, "job.created", map[string]any{
		"type": job.Type, "tasks": len(taskIDs),
	})
	obs.FromContext(ctx).InfoContext(ctx, "job created",
		obs.FieldJobID, job.ID, "type", job.Type, "tasks", len(taskIDs))

	fresh, err := s.Jobs.GetJob(ctx, job.ID)
	if err != nil {
		return nil, err
	}
	ids, _ := s.Jobs.MachineIDs(ctx, job.ID)
	return ptrJob(jobOut(fresh, ids)), nil
}

type createJobRecord struct {
	jobType         string
	flow            string
	machineIDs      []string
	actionRaw       json.RawMessage
	specRaw         json.RawMessage
	policy          store.Policy
	requestRaw      json.RawMessage
	idempotencyKey  string
	createdBy       string
	hostnamePattern *string
	overrides       map[string]gen.InstallSpec
}

func ptrJob(j gen.Job) *gen.Job { return &j }

// verifyMachinesExist rejects unknown targets up front (422, listing them).
func (s *Server) verifyMachinesExist(ctx context.Context, ids []string) error {
	var missing []string
	for _, id := range ids {
		if _, err := s.Machines.Get(ctx, id); err != nil {
			missing = append(missing, id)
		}
	}
	if len(missing) > 0 {
		return verr("SCHEMA_UNKNOWN_MACHINE", "unknown machine ids: %v", missing)
	}
	return nil
}

// encodeAction converts the contract union into storage JSON.
func encodeAction(body *gen.ActionRequest) (json.RawMessage, string, error) {
	b, err := json.Marshal(body)
	if err != nil {
		return nil, "", verr("SCHEMA_INVALID_ACTION", "action is not valid: %v", err)
	}
	var probe struct {
		Type string `json:"type"`
	}
	_ = json.Unmarshal(b, &probe)
	switch probe.Type {
	case "discover", "power_on", "power_off", "soft_off", "reboot", "hard_reboot",
		"cycle", "set_boot_device", "mount_media", "eject_media", "set_bios_attributes",
		"erase_drives":
		return b, probe.Type, nil
	default:
		return nil, "", verr("SCHEMA_INVALID_ACTION", "unknown action type %q", probe.Type)
	}
}

// validateBiosConfirm is the two-stage contract's API half (docs/07-bmc.md
// §6): a set_bios_attributes request must be non-empty and, when the
// deployment policy requires it, carry the explicit confirm flag. The
// runner adds the live-table validation on top (unknown names are rejected
// there, against the controller's actual attribute registry).
func (s *Server) validateBiosConfirm(actionRaw json.RawMessage, aType string) error {
	if aType != "set_bios_attributes" {
		return nil
	}
	var a struct {
		Attributes map[string]any `json:"attributes"`
		Confirm    bool           `json:"confirm"`
	}
	if err := json.Unmarshal(actionRaw, &a); err != nil {
		return verr("SCHEMA_INVALID_ACTION", "action is not valid: %v", err)
	}
	if len(a.Attributes) == 0 {
		return verr("SCHEMA_INVALID_ACTION", "set_bios_attributes requires a non-empty attributes object")
	}
	if s.BiosConfirmRequired && !a.Confirm {
		return verrStatus(422, "BIOS_CONFIRM_REQUIRED",
			"set_bios_attributes is high-risk and requires \"confirm\": true (deployment policy %q)", "required")
	}
	return nil
}

// validateEraseConfirm is the two-stage contract's API half for the most
// destructive action in the contract (docs/07-bmc.md §6.2): erase_drives
// must name at least one drive (serials or all) and, when the deployment
// policy requires it, carry the explicit confirm flag. The runner adds the
// live-table validation on top: any serial missing from the controller's
// physical-drive view aborts the request before the first erase starts.
func (s *Server) validateEraseConfirm(actionRaw json.RawMessage, aType string) error {
	if aType != "erase_drives" {
		return nil
	}
	var a struct {
		Serials []string `json:"serials"`
		All     bool     `json:"all"`
		Confirm bool     `json:"confirm"`
	}
	if err := json.Unmarshal(actionRaw, &a); err != nil {
		return verr("SCHEMA_INVALID_ACTION", "action is not valid: %v", err)
	}
	if len(a.Serials) == 0 && !a.All {
		return verr("SCHEMA_INVALID_ACTION",
			"erase_drives requires serials or all=true")
	}
	if len(a.Serials) > 0 && a.All {
		return verr("SCHEMA_INVALID_ACTION",
			"erase_drives takes serials or all, not both")
	}
	if s.EraseConfirmRequired && !a.Confirm {
		return verrStatus(422, "DRIVE_ERASE_CONFIRM_REQUIRED",
			"erase_drives is destructive and irreversible — it requires \"confirm\": true (deployment policy %q)", "required")
	}
	return nil
}

// validateInstallSpec: the structural half of the storage constraints from
// docs/04-install-spec.md §5.1. Layout-bound checks (preserve hits, overlap
// with keep) need snapshots and bind at install time (M2/M4).
func validateInstallSpec(spec *gen.InstallSpec) error {
	if spec.Image.Distro == "" {
		return verr("SCHEMA_INVALID_SPEC", "image.distro must be declared explicitly")
	}
	if spec.Image.Source == nil {
		return verr("SCHEMA_INVALID_SPEC", "image.source is required")
	}
	if spec.Storage == nil {
		return verr("SCHEMA_INVALID_SPEC", "storage is required")
	}
	for i, disk := range spec.Storage.Disks {
		seenRest := false
		keepKind := ""
		if disk.Keep != nil {
			keepKind = string(*disk.Keep)
		}
		partitions := derefOr(disk.Partitions, nil)
		// docs/04-install-spec.md §5.1: keep: disk excludes partitions and
		// wipe; keep: partitions COEXISTS with partitions (preserved entries
		// reuse blocks, the rest of the space is rebuilt — example ③).
		if keepKind == "disk" && len(partitions) > 0 {
			return verr("SCHEMA_INVALID_STORAGE", "disks[%d]: keep: disk excludes partitions", i)
		}
		if keepKind != "" && disk.Wipe != nil && *disk.Wipe {
			return verr("SCHEMA_INVALID_STORAGE", "disks[%d]: keep and wipe are mutually exclusive", i)
		}
		if keepKind == "" && len(partitions) == 0 {
			return verr("SCHEMA_INVALID_STORAGE", "disks[%d]: declare partitions or keep", i)
		}
		for j, part := range partitions {
			if part.Size == "" {
				return verr("SCHEMA_INVALID_STORAGE", "disks[%d].partitions[%d]: size required", i, j)
			}
			if part.Size == "rest" {
				if seenRest {
					return verr("SCHEMA_INVALID_STORAGE", "disks[%d]: at most one 'rest' partition per disk", i)
				}
				seenRest = true
			}
		}
	}
	return nil
}

func mustJSON(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}

// subject identifies the caller for audit columns (static-token era: the
// token's configured label).
func (s *Server) subject(ctx context.Context) string {
	return "api-token"
}

// ── job / task reads & transitions ──────────────────────────────────────────

func (s *Server) GetJob(ctx context.Context, request gen.GetJobRequestObject) (gen.GetJobResponseObject, error) {
	job, err := s.Jobs.GetJob(ctx, string(request.Id))
	if err != nil {
		return nil, err
	}
	ids, err := s.Jobs.MachineIDs(ctx, job.ID)
	if err != nil {
		return nil, err
	}
	return gen.GetJob200JSONResponse(jobOut(job, ids)), nil
}

func (s *Server) ListJobs(ctx context.Context, request gen.ListJobsRequestObject) (gen.ListJobsResponseObject, error) {
	p := request.Params
	items, next, err := s.Jobs.ListJobs(ctx, store.JobListFilter{
		Type:     string(derefOr(p.Type, gen.JobType(""))),
		State:    string(derefOr(p.State, gen.JobState(""))),
		PageSize: int(derefOr(p.PageSize, 50)),
		Cursor:   decodeCursor(p.Cursor),
		Order:    orderOf((*string)(p.Order)),
	})
	if err != nil {
		return nil, err
	}
	out := gen.JobList{Items: []gen.Job{}}
	for _, j := range items {
		ids, _ := s.Jobs.MachineIDs(ctx, j.ID)
		out.Items = append(out.Items, jobOut(j, ids))
	}
	out.NextCursor = encodeCursor(next)
	return gen.ListJobs200JSONResponse(out), nil
}

func (s *Server) ListJobTasks(ctx context.Context, request gen.ListJobTasksRequestObject) (gen.ListJobTasksResponseObject, error) {
	if _, err := s.Jobs.GetJob(ctx, string(request.Id)); err != nil {
		return nil, err
	}
	p := request.Params
	items, next, err := s.Jobs.ListTasks(ctx, store.TaskListFilter{
		JobID:    string(request.Id),
		State:    string(derefOr(p.State, gen.TaskState(""))),
		PageSize: int(derefOr(p.PageSize, 50)),
		Cursor:   decodeCursor(p.Cursor),
	})
	if err != nil {
		return nil, err
	}
	out := gen.TaskList{Items: []gen.Task{}}
	for _, t := range items {
		stages, _ := s.Jobs.Stages(ctx, t.ID)
		out.Items = append(out.Items, taskOut(t, stages))
	}
	out.NextCursor = encodeCursor(next)
	return gen.ListJobTasks200JSONResponse(out), nil
}

func (s *Server) GetJobTask(ctx context.Context, request gen.GetJobTaskRequestObject) (gen.GetJobTaskResponseObject, error) {
	task, err := s.Jobs.GetTask(ctx, string(request.TaskId))
	if err != nil {
		return nil, err
	}
	if task.JobID != string(request.Id) {
		return nil, store.ErrNotFound
	}
	stages, err := s.Jobs.Stages(ctx, task.ID)
	if err != nil {
		return nil, err
	}
	return gen.GetJobTask200JSONResponse(taskOut(task, stages)), nil
}

// CancelJob stops scheduling and compensates: pending/interrupted tasks are
// canceled by the control plane (its one sanctioned write), running tasks are
// flagged and their owner performs compensation (docs/02-architecture.md §2.5).
func (s *Server) CancelJob(ctx context.Context, request gen.CancelJobRequestObject) (gen.CancelJobResponseObject, error) {
	job, err := s.Jobs.GetJob(ctx, string(request.Id))
	if err != nil {
		return nil, err
	}
	switch job.State {
	case "succeeded", "failed", "canceled", "partial":
		return nil, verr("JOB_ALREADY_FINISHED", "job %s is already finished (%s)", job.ID, job.State)
	}
	flagged, err := s.Jobs.CancelJobTasks(ctx, job.ID)
	if err != nil {
		return nil, err
	}
	if err := s.Jobs.RecomputeJob(ctx, job.ID); err != nil {
		return nil, err
	}
	s.Events.Append(ctx, "job", job.ID, "job.cancel_requested", map[string]any{"flagged_running": len(flagged)})
	obs.FromContext(ctx).InfoContext(ctx, "job cancel requested",
		obs.FieldJobID, job.ID, "flagged_running", len(flagged))

	fresh, err := s.Jobs.GetJob(ctx, job.ID)
	if err != nil {
		return nil, err
	}
	ids, _ := s.Jobs.MachineIDs(ctx, job.ID)
	return gen.CancelJob202JSONResponse(jobOut(fresh, ids)), nil
}

// RetryJobTask re-enqueues a failed or interrupted task from its last stage.
func (s *Server) RetryJobTask(ctx context.Context, request gen.RetryJobTaskRequestObject) (gen.RetryJobTaskResponseObject, error) {
	task, err := s.Jobs.GetTask(ctx, string(request.TaskId))
	if err != nil {
		return nil, err
	}
	if task.JobID != string(request.Id) {
		return nil, store.ErrNotFound
	}
	switch task.State {
	case "failed", "interrupted":
		// retryable
	case "pending", "running":
		return nil, verr("JOB_TASK_ACTIVE", "task %s is %s; only failed/interrupted tasks can be retried", task.ID, task.State)
	default:
		return nil, verr("JOB_TASK_NOT_RETRYABLE", "task %s is %s and cannot be retried", task.ID, task.State)
	}
	if err := s.Jobs.ResetForRetry(ctx, task.ID); err != nil {
		return nil, err
	}
	if err := s.Queue.Enqueue(ctx, provision.QueueTasks, provision.MarshalMessage(ctx, task.ID), queue.EnqueueOptions{}); err != nil {
		return nil, verr("JOB_ENQUEUE_FAILED", "retry enqueue failed: %v", err)
	}
	s.Events.Append(ctx, "task", task.ID, "task.retry_requested", nil)
	obs.FromContext(ctx).InfoContext(ctx, "task retry enqueued", obs.FieldTaskID, task.ID)

	fresh, err := s.Jobs.GetTask(ctx, task.ID)
	if err != nil {
		return nil, err
	}
	stages, err := s.Jobs.Stages(ctx, task.ID)
	if err != nil {
		return nil, err
	}
	_ = s.Jobs.RecomputeJob(ctx, task.JobID)
	return gen.RetryJobTask202JSONResponse(taskOut(fresh, stages)), nil
}

// newToken generates the machine-facing task credential.
func newToken() (string, error) {
	var b [24]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("token entropy: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

func itoa(v int) string { return strconv.Itoa(v) }

// mergeSpecOverride merges a per-machine Install Spec fragment over the base
// spec (docs/04-install-spec.md §5: shallow, per-machine). Merging happens on
// the contract structs with zero-value detection: a fragment that did not set
// a top-level field (e.g. image) must not wipe the base value — decoding the
// fragment into the contract struct yields zero values for required fields.
func mergeSpecOverride(base json.RawMessage, overrides map[string]gen.InstallSpec, machineID string) json.RawMessage {
	ov, ok := overrides[machineID]
	if !ok || len(base) == 0 {
		return nil
	}
	var b gen.InstallSpec
	if err := json.Unmarshal(base, &b); err != nil {
		return nil
	}
	if ov.Image.Distro != "" || ov.Image.Source != nil || ov.Image.Checksum != nil {
		b.Image = ov.Image
	}
	if ov.Storage != nil {
		b.Storage = ov.Storage
	}
	if ov.Network != nil {
		b.Network = ov.Network
	}
	if ov.Identity != nil {
		b.Identity = ov.Identity
	}
	if ov.Access != nil {
		b.Access = ov.Access
	}
	if ov.Scripts != nil {
		b.Scripts = ov.Scripts
	}
	merged, err := json.Marshal(b)
	if err != nil {
		return nil
	}
	return merged
}

func isZeroImage(i gen.InstallSpec) bool { return false } // placeholder

// appErrType is the errors.As target for coded application errors.
func appErrType() *validationError { return &validationError{} }

func appErrCode(err error) string {
	var ve *validationError
	if errors.As(err, &ve) {
		return ve.code
	}
	return "LAYOUT_SNAPSHOT_REQUIRED"
}

// validateSnapshotBinding: a spec declaring keep: partitions / preserve must
// hit the machine's latest layout snapshot at submit time
// (docs/04-install-spec.md §5.1; strict per-disk resolution repeats at
// verify_layout when hardware is present).
func (s *Server) validateSnapshotBinding(ctx context.Context, machineID string, specRaw json.RawMessage) error {
	var spec struct {
		Storage *struct {
			Disks []struct {
				Select struct {
					Match struct {
						Serial string `json:"serial"`
					} `json:"match"`
				} `json:"select"`
				Keep      string `json:"keep"`
				Paritions []any  `json:"partitions"`
				Preserve  []struct {
					Number int `json:"number"`
				} `json:"preserve"`
			} `json:"disks"`
		} `json:"storage"`
	}
	if err := json.Unmarshal(specRaw, &spec); err != nil {
		return nil // structural errors surface via the schema validation path
	}
	if spec.Storage == nil {
		return nil
	}
	needs := false
	for _, d := range spec.Storage.Disks {
		if d.Keep == "partitions" || len(d.Preserve) > 0 {
			needs = true
		}
	}
	if !needs {
		return nil
	}
	content, _, err := s.Machines.LatestLayout(ctx, machineID)
	if err != nil {
		return verr("LAYOUT_SNAPSHOT_REQUIRED",
			"machine %s has no layout snapshot; capture one (in-band) before submitting keep:partitions", machineID)
	}
	var snap struct {
		Disks []struct {
			Device     string            `json:"device"`
			Match      map[string]string `json:"match"`
			Partitions []struct {
				Number int `json:"number"`
			} `json:"partitions"`
		} `json:"disks"`
	}
	if json.Unmarshal(content, &snap) != nil {
		return verr("LAYOUT_SNAPSHOT_REQUIRED", "machine %s layout snapshot is malformed", machineID)
	}
	for i, d := range spec.Storage.Disks {
		if d.Keep != "partitions" && len(d.Preserve) == 0 {
			continue
		}
		// Resolve the referenced disk in the snapshot: by serial when the
		// selector pins it, otherwise any snapshot disk carrying the numbers.
		var diskHit *int
		for di := range snap.Disks {
			sd := &snap.Disks[di]
			if d.Select.Match.Serial != "" {
				if sd.Match == nil || sd.Match["serial"] != d.Select.Match.Serial {
					continue
				}
			}
			for _, p := range d.Preserve {
				found := false
				for _, sp := range sd.Partitions {
					if sp.Number == p.Number {
						found = true
						break
					}
				}
				if !found {
					return verr("SCHEMA_INVALID_STORAGE",
						"storage.disks[%d]: preserve partition %d not present in %s snapshot (%s)",
						i, p.Number, machineID, sd.Device)
				}
			}
			diskHit = &di
			break
		}
		if diskHit == nil {
			return verr("LAYOUT_SNAPSHOT_REQUIRED",
				"storage.disks[%d]: selector does not match any disk in machine %s snapshot", i, machineID)
		}
	}
	return nil
}

// validateBootStrategy gates boot.strategy=pxe submissions: the distro must
// declare network-boot support and this deployment must run the netboot
// service (docs/06-install-pipeline.md §3.3, §6 — 拒绝在提交时,而不是装到
// 一半停在 PXE 提示符). Unknown strategies are rejected here too.
func (s *Server) validateBootStrategy(specRaw json.RawMessage) error {
	var spec struct {
		Boot *struct {
			Strategy string `json:"strategy"`
		} `json:"boot"`
		Image struct {
			Distro string `json:"distro"`
		} `json:"image"`
	}
	if err := json.Unmarshal(specRaw, &spec); err != nil {
		return nil // structural errors surface via the schema validation path
	}
	if spec.Boot == nil {
		return nil
	}
	switch spec.Boot.Strategy {
	case "", "virtual_media":
		return nil
	case "pxe":
		// fall through to the capability checks
	default:
		return verr("SCHEMA_INVALID_BOOT_STRATEGY",
			"boot.strategy %q is not one of virtual_media|pxe", spec.Boot.Strategy)
	}
	if !s.NetbootEnabled {
		return verr("SCHEMA_UNSUPPORTED_BOOT_STRATEGY",
			"boot.strategy=pxe needs the netboot service enabled on this deployment (MAMMOTH_PXE_ENABLED)")
	}
	driver, err := s.Render.For(spec.Image.Distro)
	if err != nil {
		return nil // unknown distro surfaces at execution with SCHEMA_UNKNOWN_DISTRO
	}
	if pxe := render.PXESupport(driver); pxe != render.SupportFull {
		return verr("SCHEMA_UNSUPPORTED_BOOT_STRATEGY",
			"distro %s declares %q PXE support: only the RHEL-lineage and wimboot installers boot over the network",
			spec.Image.Distro, pxe)
	}
	// The wimboot carrier's install source is the deployment SMB export —
	// without it WinPE boots and then has nothing to install from, so the
	// submission rejects up front (the NFS media export analog).
	if carrier, _ := render.NetbootInstallOf(driver); carrier == render.NetbootCarrierWimboot && !s.WindowsInstallSMBShare {
		return verr("SCHEMA_WINDOWS_SMB_SHARE_REQUIRED",
			"distro %s boots PXE through the wimboot carrier, which needs the deployment SMB export (set MAMMOTH_WINDOWS_INSTALL_SMB_SHARE)",
			spec.Image.Distro)
	}
	return nil
}

// validateFirmwareMatch gates the distro media's firmware range against the
// machine's observed PXE firmware (docs/09-roadmap.md, boot 策略门禁): a
// UEFI-only media (rocky10) handed to a BIOS-firmware machine fails at boot
// — and only at boot, possibly a batch sibling already wiped — so the
// submission rejects it instead. No observation → no gate: the check never
// blocks a machine mammoth has not yet seen on the wire, and the firmware
// fact is a point-in-time observation (docs/08-data-model.md
// machines.pxe_firmware), not a promise about the BMC boot mode right now.
func (s *Server) validateFirmwareMatch(ctx context.Context, machineID string, specRaw json.RawMessage) error {
	var spec struct {
		Image struct {
			Distro string `json:"distro"`
		} `json:"image"`
	}
	if err := json.Unmarshal(specRaw, &spec); err != nil {
		return nil // structural errors surface via the schema validation path
	}
	driver, err := s.Render.For(spec.Image.Distro)
	if err != nil {
		return nil // unknown distro surfaces at execution with SCHEMA_UNKNOWN_DISTRO
	}
	sup := render.FirmwareSupportOf(driver)
	if sup == render.FirmwareAll {
		return nil // the common case — no machine lookup at all
	}
	m, err := s.Machines.Get(ctx, machineID)
	if err != nil {
		return nil // machine state surfaces elsewhere; never double-fail here
	}
	if m.PXEFirmware == nil || sup.Allows(*m.PXEFirmware) {
		return nil
	}
	return verr("SCHEMA_FIRMWARE_MISMATCH",
		"machine %s was last seen announcing %q firmware, but distro %s media is %q — it cannot boot this media",
		machineID, *m.PXEFirmware, spec.Image.Distro, sup)
}

// validateDistroKeepSupport gates keep semantics by the distro driver's
// declared support level (docs/06-install-pipeline.md §5: 不支持分区级保留的
// 发行版在提交时即拒绝,而不是装到一半失败).
func (s *Server) validateDistroKeepSupport(specRaw json.RawMessage) error {
	var spec struct {
		Image struct {
			Distro string `json:"distro"`
		} `json:"image"`
		Storage *struct {
			Disks []struct {
				Keep     string `json:"keep"`
				Preserve []any  `json:"preserve"`
			} `json:"disks"`
		} `json:"storage"`
	}
	if err := json.Unmarshal(specRaw, &spec); err != nil {
		return nil // structural errors surface via the schema validation path
	}
	if spec.Storage == nil {
		return nil
	}
	keepDisk, keepParts := false, false
	for _, d := range spec.Storage.Disks {
		switch d.Keep {
		case "disk":
			keepDisk = true
		case "partitions":
			keepParts = true
		}
		if len(d.Preserve) > 0 {
			keepParts = true
		}
	}
	if !keepDisk && !keepParts {
		return nil
	}
	driver, err := s.Render.For(spec.Image.Distro)
	if err != nil {
		return nil // unknown distro surfaces at execution with SCHEMA_UNKNOWN_DISTRO
	}
	switch driver.KeepPartitionSupport() {
	case render.SupportFull:
		return nil
	case render.SupportPartial:
		if keepParts {
			return verr("SCHEMA_UNSUPPORTED_KEEP",
				"distro %s declares partial keep support: keep: disk is supported, keep: partitions/preserve is not",
				spec.Image.Distro)
		}
		return nil
	default:
		return verr("SCHEMA_UNSUPPORTED_KEEP",
			"distro %s does not support keep semantics", spec.Image.Distro)
	}
}
