package redfish

// Task-state classification shared by every 202-driven flow (volume
// create, virtual media mount, secure erase, BIOS settings poll).
//
// Terminal states are a blacklist, not a whitelist: HDM (AMI MegaRAC)
// documents in-flight states far beyond New/Running — Uploading,
// Verifying, Updating, Downloading, Mounting — and whitelist-style
// checks treat those as terminal, reporting success before the
// operation settles (docs/compat/h3c.md §6; same failure class as
// ejecting the installer medium before the firmware has read it).
// An unknown-but-terminal state only burns the poll deadline; an
// unknown in-flight state reported as terminal is a silent failure.
func taskTerminal(state string) bool {
	switch state {
	case "Completed", "Mounted", "Killed", "Exception", "Cancelled",
		"Failed", "Waiting For Effect", "Going To Effect":
		return true
	}
	return false
}

// taskFailed reports whether a terminal state means the operation did
// not complete — anything beyond the success states Completed and
// Mounted (some mount tasks end in the latter).
func taskFailed(state string) bool {
	return state != "Completed" && state != "Mounted"
}
