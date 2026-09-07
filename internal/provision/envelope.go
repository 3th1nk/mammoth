package provision

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/3th1nk/mammoth/internal/obs"
)

// QueueTasks is the single instruction queue for task execution.
const QueueTasks = "tasks"

// Message is the task-queue payload envelope. The trace map carries the OTel
// span context across process facets so a task stays one trace from API to
// BMC call (docs/02-architecture.md §5.2).
type Message struct {
	TaskID string            `json:"task_id"`
	Trace  map[string]string `json:"trace,omitempty"`
}

// MarshalMessage builds the envelope, injecting the current span context.
func MarshalMessage(ctx context.Context, taskID string) []byte {
	m := Message{TaskID: taskID, Trace: obs.InjectTraceContext(ctx)}
	b, _ := json.Marshal(m)
	return b
}

// UnmarshalMessage parses an envelope.
func UnmarshalMessage(payload []byte) (Message, error) {
	var m Message
	if err := json.Unmarshal(payload, &m); err != nil {
		return m, fmt.Errorf("provision: bad message: %w", err)
	}
	if m.TaskID == "" {
		return m, fmt.Errorf("provision: message without task_id")
	}
	return m, nil
}
