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

	s.WindowsInstallSMBUNC = true
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

// boot.installer=agent opts into the windows apply-image pathway: it
// consumes the HTTP win tree, so the SMB gate does not apply; the value is
// windows-only, and unknown values reject with a dedicated code.
func TestValidateBootStrategyAgentInstaller(t *testing.T) {
	reg := render.NewRegistry()
	if err := reg.Register(windows.New("windows2019")); err != nil {
		t.Fatal(err)
	}
	reg2 := render.NewRegistry()
	if err := reg2.Register(kickstart.New("rocky9")); err != nil {
		t.Fatal(err)
	}
	s := &Server{Deps{Render: reg, NetbootEnabled: true}}
	sWinAgent := &Server{Deps{Render: reg, NetbootEnabled: true, WindowsAgentInstaller: true}}
	sLinux := &Server{Deps{Render: reg2, NetbootEnabled: true}}

	t.Run("agent skips the SMB gate", func(t *testing.T) {
		spec := json.RawMessage(`{"boot":{"strategy":"pxe","installer":"agent"},"image":{"distro":"windows2019"}}`)
		if err := s.validateBootStrategy(spec); err != nil {
			t.Fatalf("agent installer rejected: %v", err)
		}
	})
	t.Run("agent is windows-only", func(t *testing.T) {
		spec := json.RawMessage(`{"boot":{"strategy":"pxe","installer":"agent"},"image":{"distro":"rocky9"}}`)
		err := sLinux.validateBootStrategy(spec)
		if err == nil {
			t.Fatal("agent installer accepted for a linux distro")
		}
		if ve, ok := err.(*validationError); !ok || ve.Code() != "SCHEMA_INVALID_BOOT_INSTALLER" {
			t.Fatalf("code = %v, want SCHEMA_INVALID_BOOT_INSTALLER", err)
		}
	})
	t.Run("unknown installer value rejects", func(t *testing.T) {
		spec := json.RawMessage(`{"boot":{"strategy":"pxe","installer":"imaging"},"image":{"distro":"windows2019"}}`)
		err := s.validateBootStrategy(spec)
		if err == nil {
			t.Fatal("unknown installer value accepted")
		}
		if ve, ok := err.(*validationError); !ok || ve.Code() != "SCHEMA_INVALID_BOOT_INSTALLER" {
			t.Fatalf("code = %v, want SCHEMA_INVALID_BOOT_INSTALLER", err)
		}
	})
	_ = sWinAgent
	_ = sLinux
}
