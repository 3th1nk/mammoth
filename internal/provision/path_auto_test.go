package provision

import (
	"testing"
)

// boot.installer=auto 的部署事实判定(影子决策落地):SMB 导出配置了 →
// setup 主线(驱动覆盖完整);未配置 → agent 通路(无 SMB 依赖)。
func TestEffectiveInstallPathAuto(t *testing.T) {
	withInstaller := func(v string) *installSpecView {
		s := &installSpecView{}
		s.Boot.Installer = v
		return s
	}
	cases := []struct {
		decl string
		smb  bool
		want installPath
	}{
		{"", false, pathSetup},
		{"", true, pathSetup},
		{"setup", true, pathSetup},
		{"agent", false, pathAgent},
		{"agent", true, pathAgent},
		{"auto", true, pathSetup},
		{"auto", false, pathAgent},
	}
	for _, c := range cases {
		got, ok := effectiveInstallPath(withInstaller(c.decl), c.smb)
		if !ok || got != c.want {
			t.Errorf("effectiveInstallPath(%q, smb=%v) = %v (ok=%v), want %v",
				c.decl, c.smb, got, ok, c.want)
		}
	}
	if _, ok := effectiveInstallPath(withInstaller("bogus"), true); ok {
		t.Errorf("unknown installer must not be known")
	}
}
