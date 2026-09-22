package api

import (
	"encoding/json"
	"testing"

	"github.com/3th1nk/mammoth/internal/render"
	"github.com/3th1nk/mammoth/internal/render/windows"
)

// boot.installer=auto 对 SMB 门禁免疫:通路判定推迟到 prepare 阶段按部署
// 事实落地(SMB 导出配置了 → setup,未配置 → agent apply),提交时不再
// 要求 SMB 导出——否则无 SMB 部署连 auto 都提交不了,判定失去意义。
func TestValidateBootStrategyAutoSkipsSMBGate(t *testing.T) {
	reg := render.NewRegistry()
	if err := reg.Register(windows.New("windows2019")); err != nil {
		t.Fatal(err)
	}
	s := &Server{Deps{Render: reg, NetbootEnabled: true}}
	spec := json.RawMessage(`{"boot":{"strategy":"pxe","installer":"auto"},"image":{"distro":"windows2019"}}`)

	s.WindowsInstallSMBUNC = false
	if err := s.validateBootStrategy(spec); err != nil {
		t.Fatalf("auto without the SMB export must pass the gate (prepare resolves to agent): %v", err)
	}
	s.WindowsInstallSMBUNC = true
	if err := s.validateBootStrategy(spec); err != nil {
		t.Fatalf("auto with the SMB export must pass the gate: %v", err)
	}
}
