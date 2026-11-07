package provision

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/3th1nk/mammoth/internal/inventory/inbandssh"
	"github.com/3th1nk/mammoth/internal/render"
	"github.com/3th1nk/mammoth/internal/store"
)

// The rescue context/report share the install context's JSON keys for the
// media facts (token/media_uri) — the shared cleanup paths parse them
// without knowing the flow — while the report lands under the "rescue" key.
func TestParseRescueContextAndRecord(t *testing.T) {
	ctxJSON := `{"token":"tok1","media_uri":"http://e/boot/boot-tok1.iso","install":{"booted_at":"2026-01-01T00:00:00Z"}}`
	task := &store.Task{Context: json.RawMessage(ctxJSON)}

	rctx := parseRescueContext(task)
	if rctx.Token != "tok1" || rctx.MediaURI == "" {
		t.Fatalf("context parse: %+v", rctx)
	}
	if rec := parseRescueRecord(task); rec != nil {
		t.Fatalf("no rescue record yet, got %+v", rec)
	}

	task.Context = json.RawMessage(`{"token":"tok1","rescue":{"ready_at":"2026-01-01T00:01:00Z","status":"ok","detail":"up"}}`)
	rec := parseRescueRecord(task)
	if rec == nil || rec.Status != "ok" || rec.Detail != "up" {
		t.Fatalf("record parse: %+v", rec)
	}
	if rec.ReadyAt == nil {
		t.Fatal("ready_at must parse")
	}

	// corrupt context: cleanup paths must never fail on it
	task.Context = json.RawMessage(`{broken`)
	if rctx := parseRescueContext(task); rctx.Token != "" {
		t.Fatalf("corrupt context must parse empty, got %+v", rctx)
	}
	if rec := parseRescueRecord(task); rec != nil {
		t.Fatalf("corrupt context must yield no record, got %+v", rec)
	}
}

// The rescue carrier decision: headless machines force PXE (no media slot),
// BMC machines default to the virtual-media mount, boot:"pxe" overrides for
// vMedia-dead firmware, anything else is a schema error.
func TestRescueCarrierFor(t *testing.T) {
	cases := []struct {
		protocol, boot, want string
		wantErr              bool
	}{
		{"redfish", "", "virtual_media", false},
		{"redfish", "virtual_media", "virtual_media", false},
		{"redfish", "pxe", "pxe", false}, // explicit override
		{"ipmi", "", "virtual_media", false},
		{"fake", "virtual_media", "virtual_media", false},
		{"none", "", "pxe", false},              // headless forces PXE
		{"none", "virtual_media", "pxe", false}, // no media slot — forced
		{"none", "pxe", "pxe", false},
		{"redfish", "usb", "", true},
		{"none", "floppy", "", true},
	}
	for _, c := range cases {
		got, err := rescueCarrierFor(c.protocol, c.boot)
		if c.wantErr {
			if err == nil {
				t.Errorf("(%s,%s): want error, got %q", c.protocol, c.boot, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("(%s,%s): unexpected error %v", c.protocol, c.boot, err)
			continue
		}
		if got != c.want {
			t.Errorf("(%s,%s): got %q, want %q", c.protocol, c.boot, got, c.want)
		}
	}
}

// The acceptance report builder (docs/06-install-pipeline.md §5): checks
// fire only where intent exists; facts unavailable degrade, mismatch fails,
// and the verdict never gates — the bond/LVM checks above it own that.
func TestBuildAcceptanceReport(t *testing.T) {
	mk := func(sys *inbandssh.SystemFacts, hostname string, disks []render.ResolvedDisk, layoutDisks []inbandssh.LayoutDisk) *inbandssh.Result {
		return &inbandssh.Result{System: sys, Layout: inbandssh.Layout{Disks: layoutDisks}}
	}

	// pass: all facts present, intents matched
	res := mk(&inbandssh.SystemFacts{Hostname: "node-1", Kernel: "6.12", OS: "Ubuntu",
		Cmdline: "BOOT_IMAGE=/vmlinuz"}, "node-1",
		[]render.ResolvedDisk{{Partitions: []render.ResolvedPartition{
			{Mount: "/"}, {Mount: "/boot/efi"}}}},
		[]inbandssh.LayoutDisk{{Partitions: []inbandssh.LayoutPartition{
			{Mountpoint: "/"}, {Mountpoint: "/boot/efi"}}}})
	ictx := &installTaskContext{Hostname: "node-1"}
	ictx.Resolved.Disks = []render.ResolvedDisk{{Partitions: []render.ResolvedPartition{
		{Mount: "/"}, {Mount: "/boot/efi"}}}}
	rep := buildAcceptanceReport(res, ictx)
	if rep.Verdict != "pass" {
		t.Fatalf("verdict = %s, want pass (%+v)", rep.Verdict, rep.Checks)
	}
	for _, c := range rep.Checks {
		if c.Status != "ok" {
			t.Errorf("check %s = %s, want ok", c.Name, c.Status)
		}
	}

	// mismatch: hostname drift → fail (evidence face only — never gates)
	res.System.Hostname = "wrong-host"
	rep = buildAcceptanceReport(res, ictx)
	if rep.Verdict != "fail" {
		t.Fatalf("mismatch verdict = %s, want fail", rep.Verdict)
	}

	// degraded: no SYS section (recorded fixture), declared mounts missing from facts
	res2 := mk(nil, "", nil, []inbandssh.LayoutDisk{{Partitions: []inbandssh.LayoutPartition{}}})
	rep = buildAcceptanceReport(res2, ictx)
	if rep.Verdict != "degraded" {
		t.Fatalf("facts-unavailable verdict = %s, want degraded (%+v)", rep.Verdict, rep.Checks)
	}

	// no declared intents (dd pathway / zero-identity): checks shrink, facts carry
	ictx2 := &installTaskContext{}
	rep = buildAcceptanceReport(res, ictx2) // res has hostname intent? none declared → hostname check absent
	names := map[string]bool{}
	for _, c := range rep.Checks {
		names[c.Name] = true
	}
	if names["hostname"] || names["storage_mounts"] {
		t.Errorf("undeclared intents must not produce checks: %+v", rep.Checks)
	}
	if !names["system_facts"] {
		t.Errorf("system_facts always fires when collected: %+v", rep.Checks)
	}
}

// The bootc/OCI install contract's submit-side gates (docs/09-roadmap.md
// 下一阶段 16): the checksum field is meaningless (the digest IS the
// integrity claim), and bootc owns the disk layout — the storage degrades
// to a single whole-disk selector, the dd shape.
func TestValidateBootc(t *testing.T) {
	mk := func() *installSpecView {
		spec := &installSpecView{}
		spec.Image.Distro = "bootc"
		spec.Image.Source = "quay.io/centos/centos-bootc:stream9"
		spec.Storage.Disks = []storageDiskView{{Wipe: true}}
		return spec
	}
	if err := validateBootc(mk()); err != nil {
		t.Fatalf("clean spec: %v", err)
	}

	spec := mk()
	spec.Image.Checksum = "sha256:" + strings.Repeat("a", 64)
	err := validateBootc(spec)
	if err == nil || !strings.Contains(err.Error(), "digest") {
		t.Fatalf("checksum must be rejected (digest-pinned source is the claim): %v", err)
	}

	spec = mk()
	spec.Storage.Disks[0].Wipe = false
	if err := validateBootc(spec); err == nil || !strings.Contains(err.Error(), "wipe") {
		t.Fatalf("unwiped target must be rejected: %v", err)
	}

	spec = mk()
	spec.Storage.Disks[0].Partitions = []partitionView{{}}
	if err := validateBootc(spec); err == nil || !strings.Contains(err.Error(), "layout") {
		t.Fatalf("declared partitions must be rejected (bootc owns the layout): %v", err)
	}

	spec = mk()
	spec.Storage.Lvm = []volumeGroupView{{}}
	if err := validateBootc(spec); err == nil {
		t.Fatal("lvm must be rejected (the image owns the layout)")
	}

	spec = mk()
	spec.Storage.Disks = []storageDiskView{{Wipe: true}, {Wipe: true}}
	if err := validateBootc(spec); err == nil || !strings.Contains(err.Error(), "one target disk") {
		t.Fatalf("multi-disk must be rejected: %v", err)
	}
}
