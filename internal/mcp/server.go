// Package mcp exposes mammoth's provisioning surface as MCP tools
// (`mammoth mcp serve`, stdio transport). It is a thin adapter over the same
// API contract the operator CLI speaks — no server-side semantics live here.
// Destructive operations (install wipes and reinstalls machines) are declared
// as such in the tool descriptions so clients can gate them.
package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"gopkg.in/yaml.v3"

	"github.com/3th1nk/mammoth/internal/apiclient"
	"github.com/3th1nk/mammoth/internal/version"
)

// Server adapts a mammoth API client into an MCP toolset.
type Server struct {
	api *apiclient.Client
}

// New builds the MCP server (tools registered, transport not yet attached).
func New(api *apiclient.Client) *mcp.Server {
	s := &Server{api: api}
	impl := &mcp.Implementation{Name: "mammoth", Version: version.Version}
	ms := mcp.NewServer(impl, nil)

	mcp.AddTool(ms, &mcp.Tool{
		Name:        "machines_list",
		Description: "List registered machines with id, BMC address, lifecycle state and power state.",
	}, s.machinesList)

	mcp.AddTool(ms, &mcp.Tool{
		Name:        "machine_get",
		Description: "Get one machine's full record: lifecycle state, power, BMC endpoint, inventory summary.",
	}, s.machineGet)

	mcp.AddTool(ms, &mcp.Tool{
		Name:        "machine_register",
		Description: "Register a machine by its out-of-band (BMC) address. Discovery starts automatically after registration.",
	}, s.machineRegister)

	mcp.AddTool(ms, &mcp.Tool{
		Name:        "machine_discover",
		Description: "Trigger (re)discovery on a machine (hardware and disk inventory). Returns a job id.",
	}, s.machineDiscover)

	mcp.AddTool(ms, &mcp.Tool{
		Name: "install_submit",
		Description: "Submit a declarative install job. DESTRUCTIVE: the target disks are wiped and the declared " +
			"OS is installed. Simplified input builds a spec with boot on the largest NVMe (ESP + swap + root); " +
			"pass raw_spec (JSON or YAML, docs/04-install-spec.md) for full control — required for windows. " +
			"Call machine_discover first: an undiscovered machine rejects installs.",
	}, s.installSubmit)

	mcp.AddTool(ms, &mcp.Tool{
		Name:        "job_get",
		Description: "Get a job's state and per-task stage progress (verify_layout → … → verify_ready).",
	}, s.jobGet)

	mcp.AddTool(ms, &mcp.Tool{
		Name:        "job_wait",
		Description: "Poll a job until it reaches a terminal state (succeeded/failed/canceled/partial) and return the final breakdown.",
	}, s.jobWait)

	mcp.AddTool(ms, &mcp.Tool{
		Name:        "job_cancel",
		Description: "Cancel a running job.",
	}, s.jobCancel)

	mcp.AddTool(ms, &mcp.Tool{
		Name:        "job_logs",
		Description: "Read a task's execution log lines (most recent last, truncated).",
	}, s.jobLogs)

	mcp.AddTool(ms, &mcp.Tool{
		Name:        "events_recent",
		Description: "Read the most recent audit events (machine/task/job lifecycle).",
	}, s.eventsRecent)

	return ms
}

// ── input types (schema is inferred: fields without omitempty are required) ─

type machinesListIn struct{}

type machineGetIn struct {
	ID string `json:"id" jsonschema:"machine id (mch_…)"`
}

type machineRegisterIn struct {
	BMCAddress   string            `json:"bmc_address" jsonschema:"out-of-band address (redfish https://host or fake://name)"`
	Protocol     string            `json:"protocol,omitempty" jsonschema:"redfish | ipmi | fake (default auto)"`
	CredentialID string            `json:"credential_id" jsonschema:"credential id (cred_…)"`
	Labels       map[string]string `json:"labels,omitempty" jsonschema:"labels as key and value pairs"`
}

type machineDiscoverIn struct {
	ID string `json:"id" jsonschema:"machine id (mch_…)"`
}

type installSubmitIn struct {
	MachineIDs      []string `json:"machine_ids" jsonschema:"target machine ids"`
	Distro          string   `json:"distro,omitempty" jsonschema:"alpine | rocky | centos | kylin | uos | ubuntu-22.04 | ubuntu-24.04 | debian-12 | debian-13 (windows needs raw_spec)"`
	ImageSource     string   `json:"image_source,omitempty" jsonschema:"install image URI (file://… or http(s)://… ISO)"`
	Strategy        string   `json:"strategy,omitempty" jsonschema:"virtual_media (default) | pxe"`
	HostnamePattern string   `json:"hostname_pattern,omitempty" jsonschema:"e.g. web-{index}"`
	SSHKeys         []string `json:"ssh_keys,omitempty" jsonschema:"authorized public keys for root on the installed system"`
	RootPassword    string   `json:"root_password,omitempty" jsonschema:"omit to let the server generate a random one"`
	RawSpec         string   `json:"raw_spec,omitempty" jsonschema:"full install spec as JSON/YAML (overrides all simplified fields)"`
}

type jobGetIn struct {
	JobID string `json:"job_id" jsonschema:"job id (job_…)"`
}

type jobWaitIn struct {
	JobID        string `json:"job_id" jsonschema:"job id (job_…)"`
	TimeoutSecs  int    `json:"timeout_seconds,omitempty" jsonschema:"give up after this long (default 900, max 3600)"`
}

type jobCancelIn struct {
	JobID string `json:"job_id" jsonschema:"job id (job_…)"`
}

type jobLogsIn struct {
	JobID  string `json:"job_id" jsonschema:"job id"`
	TaskID string `json:"task_id,omitempty" jsonschema:"task id (tsk_…); defaults to the first task"`
}

type eventsRecentIn struct {
	Count int `json:"count,omitempty" jsonschema:"how many events (default 20, max 100)"`
}

// ── handlers ────────────────────────────────────────────────────────────────

func (s *Server) machinesList(ctx context.Context, req *mcp.CallToolRequest, _ machinesListIn) (*mcp.CallToolResult, any, error) {
	var out struct {
		Items []apiclient.Machine `json:"items"`
	}
	if err := s.api.Get("/api/v1/machines?page_size=200", &out); err != nil {
		return nil, nil, err
	}
	rows := make([]map[string]any, 0, len(out.Items))
	for _, m := range out.Items {
		rows = append(rows, map[string]any{
			"id": m.ID, "bmc": m.BMC.Address, "state": m.State, "power": m.PowerState,
		})
	}
	return nil, map[string]any{"count": len(rows), "machines": rows}, nil
}

func (s *Server) machineGet(ctx context.Context, req *mcp.CallToolRequest, in machineGetIn) (*mcp.CallToolResult, any, error) {
	var m map[string]any
	if err := s.api.Get("/api/v1/machines/"+in.ID, &m); err != nil {
		return nil, nil, err
	}
	return nil, m, nil
}

func (s *Server) machineRegister(ctx context.Context, req *mcp.CallToolRequest, in machineRegisterIn) (*mcp.CallToolResult, any, error) {
	body := map[string]any{
		"bmc": map[string]any{
			"address":       in.BMCAddress,
			"protocol":      in.Protocol,
			"credential_id": in.CredentialID,
		},
	}
	if body["bmc"].(map[string]any)["protocol"] == "" {
		body["bmc"].(map[string]any)["protocol"] = "auto"
	}
	if len(in.Labels) > 0 {
		body["labels"] = in.Labels
	}
	var m map[string]any
	if err := s.api.Post("/api/v1/machines", body, &m); err != nil {
		return nil, nil, err
	}
	return nil, map[string]any{"id": m["id"], "state": m["state"]}, nil
}

func (s *Server) machineDiscover(ctx context.Context, req *mcp.CallToolRequest, in machineDiscoverIn) (*mcp.CallToolResult, any, error) {
	var out map[string]any
	if err := s.api.Post("/api/v1/machines/"+in.ID+"/actions", map[string]any{"type": "discover"}, &out); err != nil {
		return nil, nil, err
	}
	return nil, map[string]any{"job": out["id"], "machine": in.ID}, nil
}

func (s *Server) installSubmit(ctx context.Context, req *mcp.CallToolRequest, in installSubmitIn) (*mcp.CallToolResult, any, error) {
	spec, err := s.buildInstallSpec(in)
	if err != nil {
		return nil, nil, err
	}
	var out map[string]any
	if err := s.api.Post("/api/v1/jobs", map[string]any{
		"type":    "install",
		"targets": map[string]any{"machine_ids": in.MachineIDs},
		"spec":    spec,
	}, &out); err != nil {
		return nil, nil, err
	}
	return nil, map[string]any{
		"job_id":  out["id"],
		"targets": in.MachineIDs,
		"note":    "wipe+install submitted; track with job_wait or job_get",
	}, nil
}

func (s *Server) jobGet(ctx context.Context, req *mcp.CallToolRequest, in jobGetIn) (*mcp.CallToolResult, any, error) {
	return s.jobSnapshot(in.JobID)
}

func (s *Server) jobWait(ctx context.Context, req *mcp.CallToolRequest, in jobWaitIn) (*mcp.CallToolResult, any, error) {
	timeout := time.Duration(in.TimeoutSecs) * time.Second
	if in.TimeoutSecs <= 0 {
		timeout = 15 * time.Minute
	}
	if timeout > time.Hour {
		timeout = time.Hour
	}
	deadline := time.Now().Add(timeout)
	for {
		state, snap, err := s.jobState(in.JobID)
		if err != nil {
			return nil, nil, err
		}
		if terminalJob(state) {
			return nil, snap, nil
		}
		if time.Now().After(deadline) {
			return nil, nil, fmt.Errorf("job %s still %s after %s (snapshot: %v)", in.JobID, state, timeout, snap)
		}
		select {
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

func (s *Server) jobCancel(ctx context.Context, req *mcp.CallToolRequest, in jobCancelIn) (*mcp.CallToolResult, any, error) {
	var out map[string]any
	if err := s.api.Post("/api/v1/jobs/"+in.JobID+"/cancel", nil, &out); err != nil {
		return nil, nil, err
	}
	return nil, map[string]any{"job_id": in.JobID, "state": out["state"]}, nil
}

func (s *Server) jobLogs(ctx context.Context, req *mcp.CallToolRequest, in jobLogsIn) (*mcp.CallToolResult, any, error) {
	taskID := in.TaskID
	if taskID == "" {
		var tasks struct {
			Items []taskView `json:"items"`
		}
		if err := s.api.Get(fmt.Sprintf("/api/v1/jobs/%s/tasks?page_size=10", in.JobID), &tasks); err != nil {
			return nil, nil, err
		}
		if len(tasks.Items) == 0 {
			return nil, map[string]any{"lines": []string{}}, nil
		}
		taskID = tasks.Items[0].ID
	}
	var out struct {
		Items []map[string]any `json:"items"`
	}
	if err := s.api.Get(fmt.Sprintf("/api/v1/jobs/%s/tasks/%s/logs?page_size=200", in.JobID, taskID), &out); err != nil {
		return nil, nil, err
	}
	lines := make([]string, 0, len(out.Items))
	for _, l := range out.Items {
		line := fmt.Sprintf("%v %v %v", l["ts"], l["level"], l["message"])
		if stage, _ := l["stage"].(string); stage != "" {
			line += " [" + stage + "]"
		}
		if len(line) > 300 {
			line = line[:300] + "…"
		}
		lines = append(lines, line)
	}
	return nil, map[string]any{"task": taskID, "lines": lines}, nil
}

func (s *Server) eventsRecent(ctx context.Context, req *mcp.CallToolRequest, in eventsRecentIn) (*mcp.CallToolResult, any, error) {
	n := in.Count
	if n <= 0 {
		n = 20
	}
	if n > 100 {
		n = 100
	}
	var out map[string]any
	if err := s.api.Get(fmt.Sprintf("/api/v1/events?page_size=%d", n), &out); err != nil {
		return nil, nil, err
	}
	return nil, out, nil
}

// ── helpers ─────────────────────────────────────────────────────────────────

type taskView struct {
	ID        string `json:"id"`
	MachineID string `json:"machine_id"`
	State     string `json:"state"`
	Stages []struct {
		Name  string `json:"name"`
		State string `json:"state"`
	} `json:"stages"`
	Error *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func terminalJob(state string) bool {
	switch state {
	case "succeeded", "failed", "canceled", "partial":
		return true
	}
	return false
}

// jobState fetches the job plus its tasks; snapshot is the summarized shape
// both job_get and job_wait return.
func (s *Server) jobState(jobID string) (string, map[string]any, error) {
	var job struct {
		State string `json:"state"`
	}
	if err := s.api.Get("/api/v1/jobs/"+jobID, &job); err != nil {
		return "", nil, err
	}
	var tasks struct {
		Items []taskView `json:"items"`
	}
	if err := s.api.Get(fmt.Sprintf("/api/v1/jobs/%s/tasks?page_size=200", jobID), &tasks); err != nil {
		return "", nil, err
	}
	rows := make([]map[string]any, 0, len(tasks.Items))
	for _, t := range tasks.Items {
		last := ""
		for _, st := range t.Stages {
			if st.State != "pending" {
				last = st.Name
			}
		}
		row := map[string]any{
			"task": t.ID, "machine": t.MachineID, "state": t.State, "stage": last,
		}
		if t.Error != nil {
			row["error"] = t.Error.Code + ": " + t.Error.Message
		}
		rows = append(rows, row)
	}
	return job.State, map[string]any{"job_id": jobID, "state": job.State, "tasks": rows}, nil
}

func (s *Server) jobSnapshot(jobID string) (*mcp.CallToolResult, any, error) {
	_, snap, err := s.jobState(jobID)
	return nil, snap, err
}

// buildInstallSpec renders the spec for install_submit: raw_spec wins
// verbatim; otherwise the simplified fields map onto the agent-path shape
// proven by scripts/agent-dev (largest NVMe, ESP+swap+root).
func (s *Server) buildInstallSpec(in installSubmitIn) (map[string]any, error) {
	if raw := strings.TrimSpace(in.RawSpec); raw != "" {
		var spec map[string]any
		if strings.HasPrefix(raw, "{") {
			if err := json.Unmarshal([]byte(raw), &spec); err != nil {
				return nil, fmt.Errorf("raw_spec JSON: %w", err)
			}
			return spec, nil
		}
		if err := yaml.Unmarshal([]byte(raw), &spec); err != nil {
			return nil, fmt.Errorf("raw_spec YAML: %w", err)
		}
		if spec == nil {
			return nil, fmt.Errorf("raw_spec parsed to nothing")
		}
		return spec, nil
	}

	if len(in.MachineIDs) == 0 {
		return nil, fmt.Errorf("machine_ids is required")
	}
	if in.Distro == "" || in.ImageSource == "" {
		return nil, fmt.Errorf("distro and image_source are required (or pass raw_spec)")
	}
	if strings.HasPrefix(strings.ToLower(in.Distro), "windows") {
		return nil, fmt.Errorf("windows installs need raw_spec (different boot shape); see docs/04-install-spec.md")
	}
	strategy := in.Strategy
	if strategy == "" {
		strategy = "virtual_media"
	}
	sshKeys := in.SSHKeys
	if sshKeys == nil {
		sshKeys = []string{}
	}
	return map[string]any{
		"boot":  map[string]any{"strategy": strategy},
		"image": map[string]any{"source": in.ImageSource, "distro": in.Distro},
		"storage": map[string]any{"disks": []any{map[string]any{
			"select": map[string]any{"match": map[string]any{"type": "nvme", "size": "largest"}},
			"wipe":   true,
			"partitions": []any{
				map[string]any{"size": "300M", "fs": "vfat", "mount": "/boot/efi", "flags": []string{"esp"}},
				map[string]any{"size": "512M", "fs": "swap"},
				map[string]any{"size": "rest", "fs": "ext4", "mount": "/"},
			},
		}}},
		"identity": map[string]any{"hostname_pattern": orDefault(in.HostnamePattern, "node-{index}")},
		"access": map[string]any{
			"ssh_keys":      sshKeys,
			"root_password": in.RootPassword,
		},
	}, nil
}

func orDefault(v, def string) string {
	if strings.TrimSpace(v) == "" {
		return def
	}
	return v
}
