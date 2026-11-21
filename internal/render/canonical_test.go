package render

import "testing"

// fakeOSDriver satisfies OSDriver with just a name — CanonicalDistro only
// consults the registry's name set.
type fakeOSDriver struct{ name string }

func (f fakeOSDriver) Distro() string { return f.name }
func (f fakeOSDriver) SupportedArchs() []Arch {
	return []Arch{ArchAMD64}
}
func (f fakeOSDriver) RenderAnswers(in InstallInputs, m MachineView) ([]AnswerFile, BootParams, error) {
	return nil, BootParams{}, nil
}
func (f fakeOSDriver) KeepPartitionSupport() SupportLevel { return SupportNone }

// canonicalRegistry mirrors the serve.go assembly's name set (the names
// CanonicalDistro resolves against).
func canonicalRegistry(t *testing.T) *Registry {
	t.Helper()
	r := NewRegistry()
	for _, name := range []string{
		"rocky9", "rocky10", "centos7", "kylinv10", "kylinv11", "uniontechos",
		"ubuntu22", "ubuntu24", "ubuntu26", "debian12", "debian13",
		"windows2019", "alpine", "dd",
	} {
		if err := r.Register(fakeOSDriver{name}); err != nil {
			t.Fatal(err)
		}
	}
	return r
}

// TestCanonicalDistro pins the namespace reconciliation: the catalog's
// detection namespace (family token + full version) maps onto the driver
// name the spec fills — the reconciliation that driver-naming B-step
// promises (2026-11-22, after "kylin" rode a render stage to
// SCHEMA_UNKNOWN_DISTRO).
func TestCanonicalDistro(t *testing.T) {
	r := canonicalRegistry(t)
	cases := []struct{ distro, version, want string }{
		// exact names pass through (idempotent on canonical input)
		{"kylinv10", "SP3", "kylinv10"},
		{"rocky9", "9.8", "rocky9"},
		{"ubuntu24", "24.04.4", "ubuntu24"},
		{"alpine", "3.22.5", "alpine"},
		{"dd", "", "dd"},
		// family + leading version number joins
		{"kylin", "v10", "kylinv10"},
		{"kylin", "v11.1", "kylinv11"},
		{"rocky", "9.8", "rocky9"},
		{"rocky", "10.0", "rocky10"},
		{"ubuntu", "24.04.4", "ubuntu24"},
		{"ubuntu", "26.04.1", "ubuntu26"},
		{"debian", "13.7.0", "debian13"},
		{"centos", "7.9", "centos7"},
		// case drift from hand-filled registrations resolves the same
		{"Kylin", "v11", "kylinv11"},
		{"Ubuntu", "22.04.5", "ubuntu22"},
		// synonym
		{"uos", "20", "uniontechos"},
		// unknown family: normalized to the lowercase convention, no invention
		{"openEuler", "24.03", "openeuler"},
		{"Kylin", "", "kylin"},
		{"SomeOS", "1.0", "someos"},
		{"", "", ""},
	}
	for _, c := range cases {
		if got := r.CanonicalDistro(c.distro, c.version); got != c.want {
			t.Errorf("CanonicalDistro(%q, %q) = %q, want %q", c.distro, c.version, got, c.want)
		}
	}
}
