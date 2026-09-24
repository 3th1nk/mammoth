package store

import (
	"encoding/json"
	"errors"
	"time"
)

// Sentinel errors — handlers map these onto RFC 9457 responses.
var (
	ErrNotFound      = errors.New("not found")
	ErrConflict      = errors.New("conflict")
	ErrIdempotentHit = errors.New("idempotency key replay")
)

// Policy controls batch execution (docs/04-install-spec.md §5).
type Policy struct {
	Concurrency        int    `json:"concurrency,omitempty"`
	OnTaskFailure      string `json:"on_task_failure,omitempty"` // continue | abort_batch
	VerifyLayout       *bool  `json:"verify_layout,omitempty"`
	TaskTimeoutSeconds int    `json:"task_timeout_seconds,omitempty"`
	// HealthGate: off | report | block — the pre-install hardware health
	// gate (docs/04-install-spec.md §5.6). Empty reads as off.
	HealthGate string `json:"health_gate,omitempty"`
}

// GateHealth reports whether the policy intercepts unhealthy-disk installs.
func (p Policy) HealthGateBlocks() bool { return p.HealthGate == "block" }

func (p Policy) AbortBatch() bool { return p.OnTaskFailure == "abort_batch" }

func (p Policy) ConcurrencyOrDefault() int {
	if p.Concurrency <= 0 {
		return 10
	}
	return p.Concurrency
}

func (p Policy) TaskTimeout() time.Duration {
	if p.TaskTimeoutSeconds <= 0 {
		return time.Hour
	}
	return time.Duration(p.TaskTimeoutSeconds) * time.Second
}

// ErrorInfo is the machine-readable failure triple (docs/03-api.md §4).
type ErrorInfo struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	Retryable bool   `json:"retryable"`
}

// Credential is the stored, encrypted credential. Plaintext secret never
// materializes in this struct.
type Credential struct {
	ID              string
	Name            string
	Type            string // bmc | ssh
	SecretEncrypted []byte
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

// Machine is the registered bare-metal asset.
type Machine struct {
	ID              string
	Labels          map[string]string
	BMCAddress      string
	BMCProtocol     string // redfish | ipmi | auto | fake
	BMCCredentialID string
	SSHCredentialID *string
	SSHAddress      string
	Vendor          *string
	Model           *string
	SerialNumber    *string
	FirmwareVersion *string
	Hardware        json.RawMessage
	// Firmware is the controller's firmware inventory (migration 00008,
	// docs/07-bmc.md §6) — nil until a discovery with the capability
	// succeeds.
	Firmware   json.RawMessage
	PowerState string // on | off | unknown
	State      string // registering | discovering | ready | error
	LastError  *ErrorInfo
	CreatedAt  time.Time
	UpdatedAt  time.Time
	// PXE responder observations (docs/08-data-model.md machines, migration
	// 00006): the firmware architecture the client last announced (option 93
	// label: bios | ia32 | uefi-x64 | uefi-arm64) and when. Observation
	// facts, not identity — nil until the machine has been seen on the wire.
	PXEFirmware   *string
	PXELastSeenAt *time.Time
}

// FlowName maps a job type to the state-machine definition executed per task
// (docs/02-architecture.md §2: stage sequence declared by exactly one flow).
func (j *Job) FlowName() string {
	switch j.Type {
	case "install":
		return "install"
	case "discover":
		return "discover"
	default:
		return "power"
	}
}

// Job is the async-operation carrier.
type Job struct {
	ID             string
	Type           string // install | power | discover
	Request        json.RawMessage
	Action         json.RawMessage // power jobs: the BMC action
	SpecResolved   json.RawMessage
	Policy         Policy
	IdempotencyKey *string
	State          string
	Summary        map[string]int
	CreatedBy      string
	CreatedAt      time.Time
	UpdatedAt      time.Time
	FinishedAt     *time.Time
}

// Task is a per-machine subtask; only the runner advances its state
// (docs/08-data-model.md iron rule 1).
type Task struct {
	ID              string
	JobID           string
	MachineID       string
	State           string
	FlowName        string
	StageIndex      int
	StageAttempt    int
	DeliveryCount   int
	StageDeadline   *time.Time
	HeartbeatAt     *time.Time
	OwnerRunner     *string
	Context         json.RawMessage
	CancelRequested bool
	Error           *ErrorInfo
	CreatedAt       time.Time
	UpdatedAt       time.Time
	FinishedAt      *time.Time
}

// Stage is one persisted step of a task.
type Stage struct {
	TaskID     string
	Seq        int
	Name       string
	State      string
	Attempt    int
	StartedAt  *time.Time
	FinishedAt *time.Time
	DurationMs *int64
}

// MachineTaskIDs returns the task→machine association pairs for job creation.
type MachineTaskIDs struct{ TaskID, MachineID string }
