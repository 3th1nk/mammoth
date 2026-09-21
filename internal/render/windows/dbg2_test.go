package windows

import (
	"os"
	"testing"

	"github.com/3th1nk/mammoth/internal/render"
)

func TestDebugDumpXML(t *testing.T) {
	in := baseInputs()
	in.Netboot = &render.NetbootInputs{
		InstallSMBUNC:       `\\198.51.100.248\mammoth-media`,
		InstallSMBImagePath: `pool-store\abc123\win\tree`,
	}
	answers, _, err := New("windows2019").RenderAnswers(in, render.MachineView{})
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range answers {
		if a.Name == "autounattend.xml" {
			os.WriteFile("/tmp/autounattend-latest.xml", []byte(a.Content), 0o644)
		}
	}
}
