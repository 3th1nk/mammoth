package render

import "testing"

func TestFirmwareSupportAllows(t *testing.T) {
	cases := []struct {
		sup FirmwareSupport
		fw  string
		ok  bool
	}{
		{FirmwareAll, "bios", true},
		{FirmwareAll, "ia32", true},
		{FirmwareAll, "uefi-x64", true},
		{FirmwareAll, "uefi-arm64", true},
		{FirmwareAll, "", true}, // unknown label never mismatches

		{FirmwareUEFIOnly, "uefi-x64", true},
		{FirmwareUEFIOnly, "uefi-arm64", true},
		{FirmwareUEFIOnly, "bios", false},
		{FirmwareUEFIOnly, "ia32", false},

		{FirmwareBIOSOnly, "bios", true},
		{FirmwareBIOSOnly, "ia32", true},
		{FirmwareBIOSOnly, "uefi-x64", false},
	}
	for _, c := range cases {
		if got := c.sup.Allows(c.fw); got != c.ok {
			t.Errorf("%q.Allows(%q) = %v, want %v", c.sup, c.fw, got, c.ok)
		}
	}
}

// bareDriver is a minimal OSDriver used to prove the optional-capability
// default: a driver without FirmwareSupport reads as FirmwareAll.
type bareDriver struct{}

func (bareDriver) Distro() string { return "bare" }
func (bareDriver) SupportedArchs() []Arch {
	return []Arch{ArchAMD64}
}
func (bareDriver) RenderAnswers(InstallInputs, MachineView) ([]AnswerFile, BootParams, error) {
	return nil, BootParams{}, nil
}
func (bareDriver) KeepPartitionSupport() SupportLevel { return SupportFull }

func TestFirmwareSupportOfDefaults(t *testing.T) {
	if got := FirmwareSupportOf(bareDriver{}); got != FirmwareAll {
		t.Errorf("default firmware support = %q, want %q", got, FirmwareAll)
	}
}
