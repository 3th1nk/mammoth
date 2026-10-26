package provision

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/3th1nk/mammoth/internal/inventory/inbandssh"

	"github.com/3th1nk/mammoth/internal/render"
	"github.com/3th1nk/mammoth/internal/render/kickstart"
	"github.com/3th1nk/mammoth/internal/render/windows"
	"github.com/3th1nk/mammoth/internal/store"
)

// windowsInstall gates verify_ready's in-band ssh probe off for the windows
// family: there is no sshd to answer it, so an on-record credential would
// only burn the wait budget into a false INSTALL_NOT_REACHABLE.
func TestWindowsInstallFamilyGate(t *testing.T) {
	reg := render.NewRegistry()
	for _, d := range []render.OSDriver{windows.New("windows2019"), kickstart.New("rocky9")} {
		if err := reg.Register(d); err != nil {
			t.Fatalf("register %s: %v", d.Distro(), err)
		}
	}
	e := &Executor{Render: reg}

	task := &store.Task{}
	job := &store.Job{SpecResolved: json.RawMessage(`{"image":{"distro":"windows2019"}}`)}
	if !e.windowsInstall(t.Context(), task, job) {
		t.Fatal("windows2019 spec not recognized as the windows family")
	}

	job.SpecResolved = json.RawMessage(`{"image":{"distro":"rocky9"}}`)
	if e.windowsInstall(t.Context(), task, job) {
		t.Fatal("rocky9 spec classified as windows")
	}

	// Unknown distro or missing spec: the gate stays open — the legacy
	// in-band path applies, and a broken spec fails loudly downstream.
	job.SpecResolved = json.RawMessage(`{"image":{"distro":"haiku"}}`)
	if e.windowsInstall(t.Context(), task, job) {
		t.Fatal("unknown distro classified as windows")
	}
	job.SpecResolved = nil
	if e.windowsInstall(t.Context(), task, job) {
		t.Fatal("missing spec classified as windows")
	}
}

// verifyBonds holds the live system against the declared bond contract
// (docs/06-install-pipeline.md §4): members by MAC, kernel mode, miimon,
// and — for active-backup — a declared active member.
func TestVerifyBonds(t *testing.T) {
	checks := []render.BondExpected{{
		Name: "bond0", SlaveMACs: []string{"aa:bb:cc:dd:ee:01", "aa:bb:cc:dd:ee:02"},
		Mode: "802.3ad", MIImon: 100,
	}}
	res := &inbandssh.Result{
		NICs: []inbandssh.NICFacts{
			{Name: "bond0", MAC: "aa:bb:cc:dd:ee:ff"},
			{Name: "eno1", MAC: "AA:BB:CC:DD:EE:01"},
			{Name: "eno2", MAC: "aa:bb:cc:dd:ee:02"},
		},
		Bonds: []inbandssh.BondFacts{{Name: "bond0", Mode: "802.3ad", MIImon: 100,
			Slaves: []string{"eno1", "eno2"}}},
	}
	if err := verifyBonds(res, checks); err != nil {
		t.Fatalf("matching bond must verify: %v", err)
	}

	// one member down (switch port off): member set mismatch
	res.Bonds[0].Slaves = []string{"eno1"}
	if err := verifyBonds(res, checks); err == nil || !strings.Contains(err.Error(), "members") {
		t.Errorf("member mismatch must fail with the sets, got %v", err)
	}

	// member unknown to the link facts
	res.Bonds[0].Slaves = []string{"eno1", "eno9"}
	if err := verifyBonds(res, checks); err == nil || !strings.Contains(err.Error(), "no link fact") {
		t.Errorf("unknown member must fail, got %v", err)
	}
	res.Bonds[0].Slaves = []string{"eno1", "eno2"}

	// declared bond absent from the machine
	res.Bonds = nil
	if err := verifyBonds(res, checks); err == nil || !strings.Contains(err.Error(), "not present") {
		t.Errorf("missing bond must fail, got %v", err)
	}

	// mode and miimon drift (the silently-dropped lacp_rate/miimon class)
	res.Bonds = []inbandssh.BondFacts{{Name: "bond0", Mode: "balance-rr", MIImon: 100,
		Slaves: []string{"eno1", "eno2"}}}
	if err := verifyBonds(res, checks); err == nil || !strings.Contains(err.Error(), "mode") {
		t.Errorf("mode mismatch must fail, got %v", err)
	}
	res.Bonds[0].Mode = "802.3ad"
	res.Bonds[0].MIImon = 0
	if err := verifyBonds(res, checks); err == nil || !strings.Contains(err.Error(), "miimon") {
		t.Errorf("miimon mismatch must fail, got %v", err)
	}

	// active-backup: an undeclared/absent active member fails
	ab := []render.BondExpected{{Name: "bond0",
		SlaveMACs: []string{"aa:bb:cc:dd:ee:01", "aa:bb:cc:dd:ee:02"},
		Mode:      "active-backup", MIImon: 100, Active: true}}
	res.Bonds = []inbandssh.BondFacts{{Name: "bond0", Mode: "active-backup", MIImon: 100,
		Slaves: []string{"eno1", "eno2"}, Active: "eno3"}}
	if err := verifyBonds(res, ab); err == nil || !strings.Contains(err.Error(), "active member") {
		t.Errorf("undeclared active member must fail, got %v", err)
	}
	res.Bonds[0].Active = "eno2"
	if err := verifyBonds(res, ab); err != nil {
		t.Fatalf("declared active member must verify: %v", err)
	}

	// arp-monitoring bond: no miimon expectation
	arp := []render.BondExpected{{Name: "bond0",
		SlaveMACs: []string{"aa:bb:cc:dd:ee:01"}, Mode: "active-backup", Active: true}}
	res.Bonds = []inbandssh.BondFacts{{Name: "bond0", Mode: "active-backup", MIImon: 0,
		Slaves: []string{"eno1"}, Active: "eno1"}}
	if err := verifyBonds(res, arp); err != nil {
		t.Fatalf("arp-monitored bond must verify without miimon: %v", err)
	}
}
