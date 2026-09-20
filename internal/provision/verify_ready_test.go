package provision

import (
	"encoding/json"
	"testing"

	"github.com/3th1nk/mammoth/internal/render"
	"github.com/3th1nk/mammoth/internal/render/kickstart"
	"github.com/3th1nk/mammoth/internal/render/windows"
	"github.com/3th1nk/mammoth/internal/store"
)

// windowsInstall gates verify_ready's in-band ssh probe off for the windows
// family: there is no sshd to answer it, so an on-record credential would
// only burn the wait budget into a false INSTALL_NOT_REACHABLE.
func TestWindowsInstallFamilyGate(t *testing.T) {
	reg := render.NewRegistry()
	for _, d := range []render.OSDriver{windows.New("windows2019"), kickstart.New("rocky9")} {
		if err := reg.Register(d); err != nil {
			t.Fatalf("register %s: %v", d.Distro(), err)
		}
	}
	e := &Executor{Render: reg}

	task := &store.Task{}
	job := &store.Job{SpecResolved: json.RawMessage(`{"image":{"distro":"windows2019"}}`)}
	if !e.windowsInstall(t.Context(), task, job) {
		t.Fatal("windows2019 spec not recognized as the windows family")
	}

	job.SpecResolved = json.RawMessage(`{"image":{"distro":"rocky9"}}`)
	if e.windowsInstall(t.Context(), task, job) {
		t.Fatal("rocky9 spec classified as windows")
	}

	// Unknown distro or missing spec: the gate stays open — the legacy
	// in-band path applies, and a broken spec fails loudly downstream.
	job.SpecResolved = json.RawMessage(`{"image":{"distro":"haiku"}}`)
	if e.windowsInstall(t.Context(), task, job) {
		t.Fatal("unknown distro classified as windows")
	}
	job.SpecResolved = nil
	if e.windowsInstall(t.Context(), task, job) {
		t.Fatal("missing spec classified as windows")
	}
}
