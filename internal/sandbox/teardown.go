package sandbox

import (
	"fmt"
)

// Down stops the whole sandbox: install/boot qemu guests, the mammoth server,
// the postgres container. Cached assets (ISO, ssh key, logs) survive so the
// next `up` is fast. Idempotent — safe on a half-built sandbox.
func Down() error {
	st, err := LoadState()
	if err != nil {
		return fmt.Errorf("no sandbox state found (%s) — nothing to tear down", statePath())
	}
	teardownState(st)
	fmt.Println("sandbox down — cached assets kept in " + Dir())
	return nil
}

func teardownState(st *State) {
	killed := false
	for _, pid := range st.InstallPIDs {
		if pidAlive(pid) {
			terminate(pid)
			killed = true
		}
	}
	for _, pid := range st.BootPIDs {
		if pidAlive(pid) {
			terminate(pid)
			killed = true
		}
	}
	if killed {
		fmt.Println("  stopped qemu guests")
	}
	if pidAlive(st.ServerPID) {
		terminate(st.ServerPID)
		fmt.Println("  stopped mammoth")
	}
	if containerRunning(st.Container) {
		stopPostgres(st.Container)
		fmt.Println("  removed the postgres container")
	}
	removeState()
}

// Status prints what of the sandbox is alive, plus machine/job state when the
// API is reachable.
func Status() error {
	st, err := LoadState()
	if err != nil {
		fmt.Println("no sandbox state — run `mammoth sandbox up` first")
		return nil
	}
	fmt.Printf("sandbox dir:   %s (created %s)\n", Dir(), st.CreatedAt.Format("2006-01-02 15:04"))
	fmt.Printf("nodes:         %d (alpine, virtual_media install)\n", st.Nodes)

	if containerRunning(st.Container) {
		fmt.Printf("postgres:      container %s running (127.0.0.1:%d)\n", st.Container, st.DBPort)
	} else {
		fmt.Printf("postgres:      not running\n")
	}

	if pidAlive(st.ServerPID) {
		fmt.Printf("mammoth:       pid %d, %s (token in state.json)\n", st.ServerPID, apiBase())
		printJobState(st)
	} else {
		fmt.Println("mammoth:       not running")
	}

	for i, pid := range st.BootPIDs {
		if pidAlive(pid) {
			fmt.Printf("guest boot %d:  pid %d — ssh -i %s/id_ed25519 -p %d root@127.0.0.1\n",
				i+1, pid, Dir(), sshBase()+i)
		}
	}
	if len(st.BootPIDs) == 0 {
		fmt.Println("guest boots:   none (no installed system second-booted)")
	}
	return nil
}

func printJobState(st *State) {
	if st.JobID == "" {
		return
	}
	api := newAPI(st.Token)
	var job struct {
		State string `json:"state"`
	}
	if err := api.get("/api/v1/jobs/"+st.JobID, &job); err != nil {
		fmt.Printf("job %s: unreachable (%v)\n", st.JobID, err)
		return
	}
	fmt.Printf("install job:   %s (%s)\n", st.JobID, job.State)
	var tasks struct {
		Items []taskView `json:"items"`
	}
	if err := api.get(fmt.Sprintf("/api/v1/jobs/%s/tasks?page_size=200", st.JobID), &tasks); err != nil {
		return
	}
	for _, t := range tasks.Items {
		last := ""
		for _, s := range t.Stages {
			if s.State != "pending" {
				last = s.Name
			}
		}
		fmt.Printf("  machine %s: %s@%s\n", t.MachineID, t.State, last)
	}
}
