package redfish

import "testing"

func TestTaskTerminal(t *testing.T) {
	// HDM (AMI MegaRAC) documents in-flight states far beyond New/Running;
	// whitelist-style checks mistook Mounting for terminal and reported
	// success before the mount settled (docs/compat/h3c.md §6). Unknown
	// states must stay in-flight.
	for _, s := range []string{"", "New", "Running", "Mounting", "Downloading",
		"Uploading", "Verifying", "Updating", "Pending", "SomeVendorState"} {
		if taskTerminal(s) {
			t.Errorf("taskTerminal(%q) = true, want in-flight", s)
		}
	}
	for _, s := range []string{"Completed", "Mounted", "Killed", "Exception",
		"Cancelled", "Failed", "Waiting For Effect", "Going To Effect"} {
		if !taskTerminal(s) {
			t.Errorf("taskTerminal(%q) = false, want terminal", s)
		}
	}
}

func TestTaskFailed(t *testing.T) {
	for _, s := range []string{"Completed", "Mounted"} {
		if taskFailed(s) {
			t.Errorf("taskFailed(%q) = true, want success", s)
		}
	}
	for _, s := range []string{"Exception", "Failed", "Cancelled", "Killed",
		"Waiting For Effect", "Going To Effect"} {
		if !taskFailed(s) {
			t.Errorf("taskFailed(%q) = false, want failure", s)
		}
	}
}

func TestRouteMiss(t *testing.T) {
	for _, tc := range []struct {
		status int
		body   string
		miss   bool
	}{
		{404, "", true},
		{405, "", true},
		{400, `{"code":"Base.1.0.ActionNotSupported"}`, true},
		// Real payload rejections are not route misses — the namespace
		// received and judged the action.
		{400, `{"code":"iBMC.1.0.FileTransferProtocolMismatch"}`, false},
		{400, `{"code":"Base.1.0.PropertyUnknown"}`, false},
		{202, `{"Id":"1","TaskState":"New"}`, false},
		{500, "", false},
	} {
		if got := routeMiss(tc.status, tc.body); got != tc.miss {
			t.Errorf("routeMiss(%d, %q) = %v, want %v", tc.status, tc.body, got, tc.miss)
		}
	}
}
