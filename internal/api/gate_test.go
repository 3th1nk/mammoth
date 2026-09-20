package api

import (
	"encoding/json"
	"testing"

	"github.com/3th1nk/mammoth/internal/render"
	"github.com/3th1nk/mammoth/internal/render/kickstart"
	"github.com/3th1nk/mammoth/internal/render/windows"
)

// The windows wimboot carrier consumes the deployment SMB export as its
// install source — a submission without the export configured boots WinPE
// into a dead end, so the gate rejects it up front; a configured export
// passes. Non-wimboot distros never touch the gate.
func TestValidateBootStrategyWimbootShareGate(t *testing.T) {
	reg := render.NewRegistry()
	if err := reg.Register(windows.New("windows2019")); err != nil {
		t.Fatal(err)
	}
	s := &Server{Deps{Render: reg, NetbootEnabled: true}}
	spec := json.RawMessage(`{"boot":{"strategy":"pxe"},"image":{"distro":"windows2019"}}`)

	err := s.validateBootStrategy(spec)
	if err == nil {
		t.Fatal("windows PXE without the install share must be rejected")
	}
	var ve *validationError
	if e, ok := err.(*validationError); ok {
		ve = e
	} else {
		t.Fatalf("error type = %T, want *validationError", err)
	}
	if ve.Code() != "SCHEMA_WINDOWS_SMB_SHARE_REQUIRED" {
		t.Fatalf("code = %q, want SCHEMA_WINDOWS_SMB_SHARE_REQUIRED", ve.Code())
	}

	s.WindowsInstallSMBShare = true
	if err := s.validateBootStrategy(spec); err != nil {
		t.Fatalf("configured share must pass the gate: %v", err)
	}
}

// The share gate is wimboot-carrier-scoped: the Linux carriers (no SMB
// source) install exactly as before, export configured or not.
func TestValidateBootStrategyNonWimbootIgnoresShareGate(t *testing.T) {
	reg := render.NewRegistry()
	if err := reg.Register(kickstart.New("rocky9")); err != nil {
		t.Fatal(err)
	}
	s := &Server{Deps{Render: reg, NetbootEnabled: true}}
	spec := json.RawMessage(`{"boot":{"strategy":"pxe"},"image":{"distro":"rocky9"}}`)
	if err := s.validateBootStrategy(spec); err != nil {
		t.Fatalf("rocky9 PXE must not require the windows share: %v", err)
	}
}
