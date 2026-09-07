package api

import (
	"context"
	"encoding/json"

	"github.com/3th1nk/mammoth/internal/api/gen"
	"github.com/3th1nk/mammoth/internal/obs"
	"github.com/3th1nk/mammoth/internal/provision"
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

	job, err := s.createJobRecord(ctx, createJobRecord{
		jobType:        string(body.Type),
		flow:           flow,
		machineIDs:     machineIDs,
		actionRaw:      actionRaw,
		specRaw:        specRaw,
		policy:         policy,
		requestRaw:     mustJSON(body),
		idempotencyKey: derefOr(request.Params.IdempotencyKey, ""),
		createdBy:      s.subject(ctx),
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
	for range in.machineIDs {
		taskIDs = append(taskIDs, store.NewID("tsk"))
	}
	input := store.CreateJobInput{
		Job:        job,
		TaskIDs:    taskIDs,
		MachineIDs: in.machineIDs,
		Stages:     provision.StageNames(in.flow),
	}
	if err := s.Jobs.CreateJobWithTasks(ctx, input); err != nil {
		return nil, err
	}

	// Enqueue one message per task; span context rides along so the task
	// stays one trace across facets.
	for i, tid := range taskIDs {
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
	jobType        string
	flow           string
	machineIDs     []string
	actionRaw      json.RawMessage
	specRaw        json.RawMessage
	policy         store.Policy
	requestRaw     json.RawMessage
	idempotencyKey string
	createdBy      string
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
		"cycle", "set_boot_device", "mount_media":
		return b, probe.Type, nil
	default:
		return nil, "", verr("SCHEMA_INVALID_ACTION", "unknown action type %q", probe.Type)
	}
}

// validateInstallSpec: the structural half of the storage constraints from
// docs/04-install-spec.md §5.1. Layout-bound checks (preserve hits, overlap
// with keep) need snapshots and bind at install time (M2/M4).
func validateInstallSpec(spec *gen.InstallSpec) error {
	if spec.Image.Distro == "" {
		return verr("SCHEMA_INVALID_SPEC", "image.distro must be declared explicitly")
	}
	if spec.Image.Source == nil && spec.Image.ImageId == nil {
		return verr("SCHEMA_INVALID_SPEC", "image.source or image.image_id is required")
	}
	if spec.Storage == nil {
		return verr("SCHEMA_INVALID_SPEC", "storage is required")
	}
	seenRest := map[string]bool{}
	for i, disk := range spec.Storage.Disks {
		keep := disk.Keep != nil && *disk.Keep != ""
		partitions := derefOr(disk.Partitions, nil)
		if keep && len(partitions) > 0 {
			return verr("SCHEMA_INVALID_STORAGE", "disks[%d]: keep and partitions are mutually exclusive", i)
		}
		if keep && disk.Wipe != nil && *disk.Wipe {
			return verr("SCHEMA_INVALID_STORAGE", "disks[%d]: keep and wipe are mutually exclusive", i)
		}
		if !keep && len(partitions) == 0 {
			return verr("SCHEMA_INVALID_STORAGE", "disks[%d]: declare partitions or keep", i)
		}
		for j, part := range partitions {
			if part.Size == "" {
				return verr("SCHEMA_INVALID_STORAGE", "disks[%d].partitions[%d]: size required", i, j)
			}
			if part.Size == "rest" {
				if seenRest["rest"] {
					return verr("SCHEMA_INVALID_STORAGE", "disks[%d]: at most one 'rest' partition per disk", i)
				}
				seenRest["rest"] = true
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
