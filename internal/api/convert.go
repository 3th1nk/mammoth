package api

import (
	"encoding/json"
	"time"

	"github.com/3th1nk/mammoth/internal/api/gen"
	"github.com/3th1nk/mammoth/internal/store"
)

// Mappers from internal store models to contract types. JSON round-trip is
// used for the semantically-schemaless jsonb blobs (hardware, layout) so the
// contract stays the authority for their shape.

func str(s string) *string { return &s }

func intp(i int) *int { return &i }

func derefOr[T any](p *T, def T) T {
	if p == nil {
		return def
	}
	return *p
}

func derefStr(p *string) *string {
	if p == nil || *p == "" {
		return nil
	}
	v := *p
	return &v
}

func jsonUnmarshal(b []byte, v any) error { return json.Unmarshal(b, v) }

func timePtr(t time.Time) *time.Time { return &t }

func credentialOut(c *store.Credential) gen.Credential {
	return gen.Credential{
		Id:        gen.CredentialId(c.ID),
		Name:      c.Name,
		Type:      gen.CredentialType(c.Type),
		CreatedAt: c.CreatedAt,
		UpdatedAt: c.UpdatedAt,
	}
}

func machineOut(m *store.Machine) gen.Machine {
	out := gen.Machine{
		Id:         gen.MachineId(m.ID),
		Labels:     m.Labels,
		Bmc:        bmcOut(m),
		PowerState: gen.PowerState(m.PowerState),
		State:      gen.MachineState(m.State),
		CreatedAt:  m.CreatedAt,
		UpdatedAt:  m.UpdatedAt,
	}
	if m.SSHCredentialID != nil {
		out.SshCredentialId = str(*m.SSHCredentialID)
	}
	if m.LastError != nil {
		out.LastError = errorInfoOut(*m.LastError)
	}
	if len(m.Hardware) > 0 {
		var hw gen.HardwareView
		if json.Unmarshal(m.Hardware, &hw) == nil {
			out.Hardware = &hw
		}
	}
	return out
}

func bmcOut(m *store.Machine) gen.BMC {
	return gen.BMC{
		Address:         m.BMCAddress,
		Protocol:        (*gen.BMCProtocol)(str(m.BMCProtocol)),
		CredentialId:    gen.CredentialId(m.BMCCredentialID),
		Vendor:          m.Vendor,
		Model:           m.Model,
		FirmwareVersion: m.FirmwareVersion,
	}
}

func errorInfoOut(e store.ErrorInfo) *gen.ErrorInfo {
	return &gen.ErrorInfo{Code: e.Code, Message: e.Message, Retryable: &e.Retryable}
}

func policyOut(p store.Policy) gen.Policy {
	return gen.Policy{
		Concurrency:        intp(p.Concurrency),
		OnTaskFailure:      (*gen.PolicyOnTaskFailure)(str(p.OnTaskFailure)),
		TaskTimeoutSeconds: intp(p.TaskTimeoutSeconds),
		VerifyLayout:       p.VerifyLayout,
	}
}

func summaryOut(s map[string]int) gen.JobSummary {
	out := gen.JobSummary{Total: s["total"]}
	out.Pending = intp(s["pending"])
	out.Running = intp(s["running"])
	out.Succeeded = intp(s["succeeded"])
	out.Failed = intp(s["failed"])
	out.Interrupted = intp(s["interrupted"])
	out.Canceled = intp(s["canceled"])
	return out
}

func actionOut(raw json.RawMessage) *gen.ActionRequest {
	if len(raw) == 0 {
		return nil
	}
	var out gen.ActionRequest
	if json.Unmarshal(raw, &out) == nil {
		return &out
	}
	return nil
}

func specOut(raw json.RawMessage) *gen.InstallSpec {
	if len(raw) == 0 {
		return nil
	}
	var out gen.InstallSpec
	if json.Unmarshal(raw, &out) == nil {
		return &out
	}
	return nil
}

func taskOut(t *store.Task, stages []*store.Stage) gen.Task {
	out := gen.Task{
		Id:        gen.TaskId(t.ID),
		JobId:     gen.JobId(t.JobID),
		MachineId: gen.MachineId(t.MachineID),
		State:     gen.TaskState(t.State),
		FlowName:  str(t.FlowName),
		Attempt:   t.StageAttempt,
		Stages:    []gen.Stage{},
		CreatedAt: t.CreatedAt,
		UpdatedAt: timePtr(t.UpdatedAt),
	}
	if t.Error != nil {
		out.Error = errorInfoOut(*t.Error)
	}
	if t.FinishedAt != nil {
		out.FinishedAt = t.FinishedAt
	}
	for _, s := range stages {
		st := gen.Stage{
			Name:       s.Name,
			State:      gen.StageState(s.State),
			Attempt:    intp(s.Attempt),
			StartedAt:  s.StartedAt,
			FinishedAt: s.FinishedAt,
		}
		if s.DurationMs != nil {
			st.DurationMs = intp(int(*s.DurationMs))
		}
		out.Stages = append(out.Stages, st)
	}
	return out
}

func jobOut(j *store.Job, machineIDs []string) gen.Job {
	out := gen.Job{
		Id:         gen.JobId(j.ID),
		Type:       gen.JobType(j.Type),
		State:      gen.JobState(j.State),
		Summary:    summaryOut(j.Summary),
		CreatedAt:  j.CreatedAt,
		FinishedAt: j.FinishedAt,
		Spec:       specOut(j.SpecResolved),
		Action:     actionOut(j.Action),
	}
	if len(machineIDs) > 0 {
		ids := make([]gen.MachineId, 0, len(machineIDs))
		for _, id := range machineIDs {
			ids = append(ids, gen.MachineId(id))
		}
		out.MachineIds = &ids
	}
	p := policyOut(j.Policy)
	out.Policy = &p
	return out
}
