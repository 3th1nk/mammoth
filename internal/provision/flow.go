// Package provision carries the execution facets: flow (state machine)
// definitions, the queue message envelope, the per-task executor, the runner
// workers, and the reaper that converts lost heartbeats into retryable
// interrupted tasks (docs/02-architecture.md).
package provision

// Flow names the state-machine definition executed per task. Stage sequences
// are declared here and nowhere else — there is exactly one source for the
// enumeration and the execution order (docs/02-architecture.md §2.1).
const (
	FlowPower    = "power"
	FlowDiscover = "discover"
	FlowInstall  = "install"
)

// StageNames returns the ordered stages of a flow.
//
//	power:    one generic BMC action
//	discover: out-of-band probe
//	install:  the five-stage pipeline (M3; fails explicitly until then)
func StageNames(flow string) []string {
	switch flow {
	case FlowPower:
		return []string{"bmc_action"}
	case FlowDiscover:
		return []string{"probe"}
	case FlowInstall:
		return []string{"verify_layout", "prepare_media", "boot", "install_os", "verify_ready"}
	default:
		return []string{"unknown"}
	}
}
