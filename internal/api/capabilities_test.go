package api

import (
	"context"
	"testing"

	"github.com/3th1nk/mammoth/internal/api/gen"
	"github.com/3th1nk/mammoth/internal/render"
	"github.com/3th1nk/mammoth/internal/render/agent"
	"github.com/3th1nk/mammoth/internal/render/bootc"
	"github.com/3th1nk/mammoth/internal/render/dd"
)

// The distro support matrix's selectable surface (docs/06-install-pipeline
// §5): pathway drivers that are frozen (bootc) or lack a client form model
// (alpine agent) ride the UserFacingDriver opt-out — they stay registered
// and API-servable, the capabilities flag just tells clients to filter.
// Undeclared drivers default to user-facing.
func TestCapabilitiesDistroUserFacing(t *testing.T) {
	reg := render.NewRegistry()
	for _, d := range []render.OSDriver{
		stubDistro("rocky9"),
		dd.New("dd"),
		agent.New("alpine"),
		bootc.New("bootc"),
	} {
		if err := reg.Register(d); err != nil {
			t.Fatalf("register %s: %v", d.Distro(), err)
		}
	}
	s := &Server{Deps: Deps{Render: reg}}

	resp, err := s.GetCapabilities(context.Background(), gen.GetCapabilitiesRequestObject{})
	if err != nil {
		t.Fatalf("capabilities: %v", err)
	}
	got, ok := resp.(gen.GetCapabilities200JSONResponse)
	if !ok {
		t.Fatalf("unexpected response type %T", resp)
	}
	if got.Distros == nil {
		t.Fatalf("distros surface missing")
	}
	flag := map[string]bool{}
	for _, d := range *got.Distros {
		flag[d.Name] = d.UserFacing == nil || *d.UserFacing
	}
	for name, want := range map[string]bool{
		"rocky9": true, // undeclared → user-facing (third-party unchanged)
		"dd":     true, // pathway driver, wizard form exists, stays selectable
		"alpine": false,
		"bootc":  false,
	} {
		if flag[name] != want {
			t.Fatalf("distro %s user_facing = %v, want %v", name, flag[name], want)
		}
	}
}

// stubDistro is the minimal OSDriver standing in for a regular distro.
type stubDistro string

func (d stubDistro) Distro() string                { return string(d) }
func (d stubDistro) SupportedArchs() []render.Arch { return []render.Arch{render.ArchAMD64} }
func (d stubDistro) RenderAnswers(render.InstallInputs, render.MachineView) ([]render.AnswerFile, render.BootParams, error) {
	return nil, render.BootParams{}, nil
}
func (d stubDistro) KeepPartitionSupport() render.SupportLevel { return render.SupportNone }
