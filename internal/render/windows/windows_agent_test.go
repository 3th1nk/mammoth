package windows

import (
	"encoding/base64"
	"strings"
	"testing"

	"github.com/3th1nk/mammoth/internal/render"
)

// The agent apply-image flavor: the carrier flips to the alpine agent
// chain, the answer set becomes plan + Panther unattend + task.json plus
// the boot-two (bcdboot WinPE) startnet pair under the wimboot seed names,
// and the kernel arguments take the alpine carrier's shape — the callback
// face (task.json contract) stays identical to the setup path so
// verify_ready cannot tell the pathways apart.
func TestRenderWindowsAgentApply(t *testing.T) {
	in := baseInputs()
	in.Installer = "agent"
	in.Netboot = &render.NetbootInputs{
		InstallWimURL:   "http://ext/netboot/store/abc/win/tree/sources/install.wim",
		InstallWimIndex: 1,
	}
	in.SpecializeXML = "<sysprepInformation><imaging>...</imaging></sysprepInformation>"
	answers, boot, err := New("windows2019").RenderAnswers(in, render.MachineView{})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	wantNames := []string{"agent-plan.sh", "agent-plan.json", "unattend-panther.xml", AgentTaskJSONName, SpecializeXMLSeedName, StartnetSeedName, WinpeshlIniSeedName}
	if len(answers) != len(wantNames) {
		t.Fatalf("answers = %d, want %d (%v)", len(answers), len(wantNames), wantNames)
	}
	got := map[string]string{}
	for _, a := range answers {
		got[a.Name] = a.Content
	}
	for _, n := range wantNames {
		if _, ok := got[n]; !ok {
			t.Errorf("answer %s missing", n)
		}
	}
	plan := got["agent-plan.sh"]
	for _, want := range []string{
		"MAMMOTH_WIN_MODE='apply'",
		"MAMMOTH_WIN_WIM='http://ext/netboot/store/abc/win/tree/sources/install.wim'",
		"MAMMOTH_WIN_INDEX='1'",
		"mammoth_disk 'sda' '-'",
		"mammoth_partition 'sda' '/boot/efi' 'vfat' '512' 'esp'",
		// the synthesized MSR between ESP and OS (final on-disk order)
		"mammoth_partition 'sda' '-' '-' '16' ''",
		"mammoth_partition 'sda' '/' 'ntfs' '-' ''",
		"mammoth_network 'aa:bb:cc:dd:ee:0a' '172.16.1.50/24,172.16.1.51/24' '172.16.1.1' '10.0.0.53,10.0.0.54'",
	} {
		if !strings.Contains(plan, want) {
			t.Errorf("plan missing %q\nplan:\n%s", want, plan)
		}
	}
	// the BCD dead-end is gone from the plan: no tree fetch, no patch tool
	for _, banned := range []string{"MAMMOTH_WIN_TREE", "MAMMOTH_WIN_BCDPATCH"} {
		if strings.Contains(plan, banned) {
			t.Errorf("plan still carries the removed BCD-patch input %s", banned)
		}
	}
	// Panther unattend: specialize + oobeSystem, NO windowsPE pass
	panther := got["unattend-panther.xml"]
	if strings.Contains(panther, "windowsPE") || strings.Contains(panther, "DiskConfiguration") {
		t.Errorf("panther unattend must not carry the windowsPE pass (setup's job)")
	}
	for _, want := range []string{"specialize", "oobeSystem", "FirstLogonCommands", "ComputerName"} {
		if !strings.Contains(panther, want) {
			t.Errorf("panther unattend missing %q", want)
		}
	}
	// boot-two startnet: native bcdboot + the NT BcdOpenStore acceptance
	// gate + the verdict marker, and it must NEVER reboot on its own (the
	// engine releases PXE and power-cycles on the marker)
	startnet := got[StartnetSeedName]
	for _, want := range []string{
		"wpeinit",
		"diskpart /s X:\\mammoth-dp.txt",
		"bcdboot %WDRV%:\\Windows /s S: /f UEFI",
		"bcdedit /store S:\\EFI\\Microsoft\\Boot\\BCD /enum all",
		BCDBootMarkerOK,
		BCDBootMarkerFail,
		"/diag/" + BCDBootDiagMarker,
		":hold",
	} {
		if !strings.Contains(startnet, want) {
			t.Errorf("bcdboot startnet missing %q\nstartnet:\n%s", want, startnet)
		}
	}
	if strings.Contains(strings.ToLower(startnet), "wpeutil reboot") {
		t.Errorf("bcdboot startnet must not self-reboot (races the PXE release)")
	}
	if got[WinpeshlIniSeedName] != winpeshlIni() {
		t.Errorf("winpeshl seed mismatch")
	}
	// specialize strip: SpBcd module removed from the sysprep action file
	if strings.Contains(got[SpecializeXMLSeedName], "SpBcd") {
		t.Errorf("Specialize.xml still carries the SpBcd block")
	}
	// task.json contract identical to the setup path
	if !strings.Contains(got[AgentTaskJSONName], "10.0.2.2:8080/render/tokw/complete") {
		t.Errorf("task.json missing the completion URL")
	}
	// alpine carrier boot shape
	for _, want := range []string{"modloop=", "alpine_repo=", "apkovl=", "mammoth_base=http://10.0.2.2:8080/render/tokw"} {
		if !strings.Contains(boot.NetbootKernelArgs, want) {
			t.Errorf("netboot args missing %q: %s", want, boot.NetbootKernelArgs)
		}
	}
	if !boot.InstallerAutoReboot {
		t.Errorf("agent apply must self-reboot")
	}
}

// The agent apply path is PXE-only and consumes the HTTP win tree — the
// SMB/pool inputs are the setup path's shape and a mixed submission is a
// spec smell rejected at render.
func TestRenderWindowsAgentApplyRejectsMixedInputs(t *testing.T) {
	in := baseInputs()
	in.Installer = "agent"
	in.Netboot = &render.NetbootInputs{InstallSMBUNC: `\\srv\media`}
	if _, _, err := New("windows2019").RenderAnswers(in, render.MachineView{}); err == nil {
		t.Fatalf("SMB-shaped netboot inputs accepted on the agent path")
	}
	in2 := baseInputs()
	in2.Installer = "agent"
	if _, _, err := New("windows2019").RenderAnswers(in2, render.MachineView{}); err == nil {
		t.Fatalf("agent render accepted without netboot inputs (PXE-only path)")
	}
	in3 := baseInputs()
	in3.Installer = "agent"
	in3.Netboot = &render.NetbootInputs{InstallWimURL: "http://ext/x.wim", InstallWimIndex: 0}
	if _, _, err := New("windows2019").RenderAnswers(in3, render.MachineView{}); err == nil {
		t.Fatalf("agent render accepted an unresolved wim index")
	}
}

// The carrier choice flips with the installer declaration: setup → wimboot
// (default), agent → alpine netboot.
func TestWindowsCarrierFollowsInstaller(t *testing.T) {
	d := New("windows2019")
	if got := d.NetbootCarrierFor(render.InstallInputs{}); got != render.NetbootCarrierWimboot {
		t.Errorf("default carrier = %q, want wimboot", got)
	}
	if got := d.NetbootCarrierFor(render.InstallInputs{Installer: "agent"}); got != render.NetbootCarrierAlpineNetboot {
		t.Errorf("agent carrier = %q, want alpine_netboot", got)
	}
}

// post_install segments ride the agent pathway's task.json too (in-wim
// injected pre-apply — the first-boot chain is byte-identical to setup);
// pre_install is rejected: the pre-apply runtime is the busybox agent, not
// WinPE (docs/04-install-spec.md §5 windows seats).
func TestRenderWindowsAgentApplyScripts(t *testing.T) {
	d := New("windows2019")
	in := baseInputs()
	in.Installer = "agent"
	in.Netboot = &render.NetbootInputs{
		InstallWimURL:   "http://ext/netboot/store/abc/win/tree/sources/install.wim",
		InstallWimIndex: 2,
	}
	in.Scripts = []render.ScriptEntry{
		{Stage: "pre_install", Inline: "echo nope\r\n"},
		{Stage: "post_install", Inline: "dir C:\\ > NUL\r\n"},
	}
	if _, _, err := d.RenderAnswers(in, render.MachineView{}); err == nil || !strings.Contains(err.Error(), "pre_install scripts are not supported on the agent apply path") {
		t.Fatalf("agent pre_install: err = %v, want agent-path rejection", err)
	}

	in.Scripts = in.Scripts[1:]
	answers, _, err := d.RenderAnswers(in, render.MachineView{})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	var task, plan string
	for _, a := range answers {
		switch a.Name {
		case AgentTaskJSONName:
			task = a.Content
		case "agent-plan.sh":
			plan = a.Content
		}
	}
	if task == "" || plan == "" {
		t.Fatalf("missing answers: %+v", answers)
	}
	if !strings.Contains(task, `"post_install"`) || !strings.Contains(task, base64.StdEncoding.EncodeToString([]byte("dir C:\\ > NUL\r\n"))) {
		t.Errorf("agent task.json missing the post_install segment:\n%s", task)
	}
	if strings.Contains(plan, "mammoth_script") {
		t.Errorf("agent plan must stay script-free (post_install runs on the installed OS, not the agent):\n%s", plan)
	}
}
